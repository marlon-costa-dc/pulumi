// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package newcmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strconv"

	survey "github.com/AlecAivazis/survey/v2"
	surveycore "github.com/AlecAivazis/survey/v2/core"

	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	cmdStack "github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/stack"
	cmdTemplates "github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/templates"
	"github.com/pulumi/pulumi/pkg/v3/cmd/pulumi/ui"
	"github.com/pulumi/pulumi/pkg/v3/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"

	pkgWorkspace "github.com/pulumi/pulumi/pkg/v3/workspace"
)

var errNoProjectCreated = errors.New("no project created; please use `pulumi new` to start again")

// setupAnswers is what `pulumi new` learned from its questions, through either the guided
// confirmation or the sequential prompts.
type setupAnswers struct {
	name        string
	description string
	// guided is set when the guided confirmation showed these answers.
	guided bool
	// stackName is the stack to create. When empty, the stack is the one named by --stack, or is
	// asked for when it is created.
	stackName string
	// config is the template config gathered for the new stack. When nil, it is asked for once the
	// stack exists.
	config *askedConfig
	// runtimeOptions answer the language runtime's questions, such as which package manager to use.
	runtimeOptions map[string]any
}

// askedConfig is template config gathered before the stack exists, held as plaintext until the
// stack's secrets manager can encrypt it.
type askedConfig struct {
	values            []templateConfigValue
	commandLineConfig config.Map
}

// askSetupQuestions asks what `pulumi new` needs to know about the project. Nothing has been written
// when it returns, so a cancelled question leaves nothing behind.
func askSetupQuestions(
	ctx context.Context, ws pkgWorkspace.Context, b backend.Backend, orgName string, existing backend.Stack,
	template cmdTemplates.ProjectTemplate, args newArgs, opts display.Options, cwd string,
) (*setupAnswers, error) {
	answers, err := askProjectQuestions(ctx, b, orgName, existing, template, args, opts, cwd)
	if err != nil {
		return nil, err
	}
	if answers.runtimeOptions, err = promptTemplateRuntimeOptions(ctx, ws, template, args, opts); err != nil {
		return nil, err
	}
	return answers, nil
}

// askProjectQuestions asks the guided or sequential project questions.
func askProjectQuestions(
	ctx context.Context, b backend.Backend, orgName string, existing backend.Stack,
	template cmdTemplates.ProjectTemplate, args newArgs, opts display.Options, cwd string,
) (*setupAnswers, error) {
	defaults := resolveProjectDefaults(ctx, b, orgName, template, args, opts, cwd)
	answers, err := confirmGuidedValues(ctx, b, orgName, defaults, template, args, opts)
	if err != nil || answers != nil {
		return answers, err
	}

	answers = &setupAnswers{}
	if answers.name, answers.description, err = promptSequentialValues(
		ctx, b, orgName, defaults, args, opts,
	); err != nil {
		return nil, err
	}
	createsStack := !args.generateOnly && existing == nil
	if createsStack && args.stack == "" {
		useProjectForStackNames(b, answers.name)
		if answers.stackName, err = promptStackName(
			args.stdout, b, args.prompt, defaultStackName, args.yes, opts, b.ValidateStackName,
		); err != nil {
			return nil, err
		}
	}
	if createsStack {
		if answers.config, err = answers.resolveConfigDefaults(template, args); err != nil {
			return nil, err
		}
		if err = askTemplateConfig(answers.config.values, args.prompt, args.yes, askAll, opts); err != nil {
			return nil, err
		}
	}
	return answers, nil
}

// promptTemplateRuntimeOptions asks the template's language runtime for its options. The language host
// runs in the template directory because the project directory is still empty; runNew starts another
// in the project directory for Template and the installs.
func promptTemplateRuntimeOptions(
	ctx context.Context, ws pkgWorkspace.Context, template cmdTemplates.ProjectTemplate,
	args newArgs, opts display.Options,
) (map[string]any, error) {
	if args.promptRuntimeOptions == nil {
		return nil, nil
	}
	proj, err := workspace.LoadProject(filepath.Join(template.Dir, "Pulumi.yaml"))
	if err != nil {
		return nil, err
	}
	if proj.Runtime.Name() == "" {
		return nil, nil
	}
	if err := setRuntimeOptionFlags(&proj.Runtime, args.runtimeOptions); err != nil {
		return nil, err
	}

	pluginHost, pluginCtx, entryPoint, err := newProjectPluginContext(ctx, ws, proj, template.Dir)
	if err != nil {
		return nil, err
	}
	defer contract.IgnoreClose(pluginHost)
	defer pluginCtx.Close()

	lang, err := pluginCtx.Host.LanguageRuntime(pluginCtx, proj.Runtime.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to load language plugin %s: %w", proj.Runtime.Name(), err)
	}
	defer lang.Close()

	return args.promptRuntimeOptions(pluginCtx, lang, &proj.Runtime, entryPoint, opts,
		args.yes, args.interactive, args.prompt)
}

// useProjectForStackNames lets b resolve stack names without a project before the project is saved.
func useProjectForStackNames(b backend.Backend, projectName string) {
	b.SetCurrentProject(&workspace.Project{Name: tokens.PackageName(projectName)})
}

// confirmedStack reports whether the guided confirmation showed the stack name.
func (a *setupAnswers) confirmedStack() bool {
	return a.guided && a.stackName != ""
}

// createStack creates the stack named by the answers, by --stack, or by a prompt, in that order. It
// returns the stack and, when the answers named it, its full name.
func (a *setupAnswers) createStack(
	ctx context.Context, sink diag.Sink, ws pkgWorkspace.Context, b backend.Backend,
	root string, args newArgs, opts display.Options,
) (backend.Stack, string, error) {
	createOpts := cmdStack.CreateStackOptions{
		SetCurrent:         true,
		SecretsProvider:    args.secretsProvider,
		UseRemoteConfig:    args.remoteStackConfig,
		ManagedEnvironment: args.escConfig,
		// The confirmation already showed the stack name, and the summary repeats it and names any
		// managed environment.
		Quiet: a.confirmedStack(),
	}

	var s backend.Stack
	var name string
	var err error
	if a.stackName != "" {
		s, name, err = createStackWithRetry(ctx, sink, args.stdout, ws, b, args.prompt, a.stackName, root,
			args.yes, opts, createOpts)
	} else {
		s, err = promptAndCreateStack(ctx, sink, ws, b, args.prompt, args.stack, root, args.yes, opts, createOpts)
	}
	if err != nil {
		return nil, "", err
	}
	if !createOpts.Quiet {
		// cmdStack.CreateStack prints "Created stack '<stack>'" on success.
		fmt.Fprintln(args.stdout)
	}
	return s, name, nil
}

// saveConfig saves the config gathered with the answers or, when there is none, asks for it and
// saves it.
func (a *setupAnswers) saveConfig(
	ctx context.Context, sink diag.Sink, ssml cmdStack.SecretsManagerLoader, ws pkgWorkspace.Context,
	proj *workspace.Project, s backend.Stack, template cmdTemplates.ProjectTemplate,
	args newArgs, opts display.Options,
) (config.Map, error) {
	if a.config != nil {
		saved, err := saveTemplateConfig(ctx, sink, ssml, ws, proj, s, a.config.values, a.config.commandLineConfig, "")
		if err != nil {
			return nil, err
		}
		// The confirmation already showed the config; otherwise report it as handleConfig does.
		if !a.guided && len(saved) > 0 {
			fmt.Fprintln(args.stdout, "Saved config")
			fmt.Fprintln(args.stdout)
		}
		return saved, nil
	}
	return handleConfig(ctx, sink, ssml, ws, args.prompt, proj, s,
		args.templateNameOrURL, template, args.configArray, args.yes, args.configPath, opts, "")
}

func promptRuntimeOptions(ctx *plugin.Context, language plugin.LanguageRuntime, info *workspace.ProjectRuntimeInfo,
	main string, opts display.Options, yes, interactive bool, prompt promptForValueFunc,
) (map[string]any, error) {
	options := make(map[string]any, len(info.Options()))
	maps.Copy(options, info.Options())

	// Keep querying for prompts until there are no more.
	for {
		// Update the program info with the latest runtime options.
		programInfo := plugin.NewProgramInfo(ctx.Root, ctx.Pwd, main, options)
		prompts, err := language.RuntimeOptionsPrompts(ctx.Request(), programInfo)
		if err != nil {
			return nil, fmt.Errorf("failed to get runtime options prompts: %w", err)
		}

		if len(prompts) == 0 {
			break
		}

		surveycore.DisableColor = true
		for _, optionPrompt := range prompts {
			if yes {
				if optionPrompt.Default == nil {
					return nil, fmt.Errorf("must provide a runtime option for '%s' in non-interactive mode", optionPrompt.Key)
				}
				options[optionPrompt.Key] = optionPrompt.Default.Value()
				continue
			}

			if len(optionPrompt.Choices) == 1 {
				// We got exactly one choice, so use that as default value without prompting the user.
				choice := optionPrompt.Choices[0]
				options[optionPrompt.Key] = choice.Value()
			} else if len(optionPrompt.Choices) > 0 {
				// Pick one among the choices
				choices := make([]string, 0, len(optionPrompt.Choices))
				// Map choice display string to the actual value
				choiceMap := make(map[string]any, len(optionPrompt.Choices))
				for _, choice := range optionPrompt.Choices {
					displayName := choice.DisplayName
					if displayName == "" {
						displayName = choice.String()
					}
					choices = append(choices, displayName)
					choiceMap[displayName] = choice.Value()
				}

				var response string
				message := opts.Color.Colorize(colors.SpecPrompt + "\r" + optionPrompt.Description + colors.Reset)
				if err := survey.AskOne(&survey.Select{
					Message: message,
					Options: choices,
				}, &response, ui.SurveyIcons(opts.Color), nil); err != nil {
					return nil, err
				}
				options[optionPrompt.Key] = choiceMap[response]
			} else {
				// Free form input
				val, err := prompt(yes, optionPrompt.Description, "", false, makePromptValidator(optionPrompt), opts)
				if err != nil {
					return nil, err
				}
				r, err := plugin.RuntimeOptionValueFromString(optionPrompt.PromptType, val)
				if err != nil {
					return nil, err
				}
				options[optionPrompt.Key] = r.Value()
			}
		}
	}

	return options, nil
}

func makePromptValidator(prompt plugin.RuntimeOptionPrompt) func(string) error {
	return func(value string) error {
		switch prompt.PromptType {
		case plugin.PromptTypeInt32:
			_, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				return fmt.Errorf("expected an integer value, got %q", value)
			}
		case plugin.PromptTypeString:
			// No validation needed for strings.
		default:
			return fmt.Errorf("unexpected prompt type: %v", prompt.PromptType)
		}
		return nil
	}
}
