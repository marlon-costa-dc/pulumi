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

package ocikms

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/keymanagement"
	"gocloud.dev/gcerrors"
	"gocloud.dev/secrets"

	"github.com/pulumi/pulumi/pkg/v3/util/ociutil"
)

func OpenKeeper(
	ctx context.Context,
	endpoint, keyid, algo string,
	configProvider common.ConfigurationProvider,
) (*secrets.Keeper, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("OCI crypto endpoint requires HTTPS without credentials, query or fragment")
	}
	if keyid == "" {
		return nil, errors.New("OCI Key Management requires a key reference")
	}
	algorithm, ok := keymanagement.GetMappingEncryptDataDetailsEncryptionAlgorithmEnum(algo)
	if !ok {
		return nil, errors.New("invalid encryption algorithm")
	}
	if configProvider == nil {
		var err error
		configProvider, err = ociutil.ConfigurationProvider(nil)
		if err != nil {
			return nil, err
		}
	}

	kmsClient, err := keymanagement.NewKmsCryptoClientWithConfigurationProvider(configProvider, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to init crypto client: %w", err)
	}
	return secrets.NewKeeper(&keeper{
		client: kmsClient, keyid: keyid, algo: string(algorithm),
	}), nil
}

type keeper struct {
	client keymanagement.KmsCryptoClient
	keyid  string
	algo   string
}

// Close implements driver.Keeper.
func (k *keeper) Close() error { return nil }

// Decrypt implements driver.Keeper.
func (k *keeper) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	encoded := string(ciphertext)
	req := keymanagement.DecryptRequest{
		DecryptDataDetails: keymanagement.DecryptDataDetails{
			KeyId:               &k.keyid,
			Ciphertext:          &encoded,
			EncryptionAlgorithm: keymanagement.DecryptDataDetailsEncryptionAlgorithmEnum(k.algo),
		},
	}
	resp, err := k.client.Decrypt(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Plaintext == nil {
		return nil, errors.New("OCI Decrypt response omitted plaintext")
	}
	return base64.StdEncoding.DecodeString(*resp.Plaintext)
}

// Encrypt implements driver.Keeper.
func (k *keeper) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	encoded := base64.StdEncoding.EncodeToString(plaintext)
	encryptReq := keymanagement.EncryptRequest{
		EncryptDataDetails: keymanagement.EncryptDataDetails{
			KeyId:               &k.keyid,
			Plaintext:           &encoded,
			EncryptionAlgorithm: keymanagement.EncryptDataDetailsEncryptionAlgorithmEnum(k.algo),
		},
	}
	resp, err := k.client.Encrypt(ctx, encryptReq)
	if err != nil {
		return nil, err
	}
	if resp.Ciphertext == nil {
		return nil, errors.New("OCI Encrypt response omitted ciphertext")
	}
	return []byte(*resp.Ciphertext), nil
}

// ErrorAs implements driver.Keeper.
func (*keeper) ErrorAs(err error, target any) bool {
	return ociutil.ErrorAs(err, target)
}

// ErrorCode implements driver.Keeper.
func (*keeper) ErrorCode(err error) gcerrors.ErrorCode {
	return ociutil.ErrorCode(err)
}
