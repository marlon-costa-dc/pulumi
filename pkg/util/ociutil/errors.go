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

// Package ociutil shares native OCI authentication and error handling.
package ociutil

import (
	"context"
	"errors"
	"net/http"

	"github.com/oracle/oci-go-sdk/v65/common"
	"gocloud.dev/gcerrors"
)

// ErrorAs exposes the native OCI service error metadata.
func ErrorAs(err error, target any) bool {
	if destination, ok := target.(*common.ServiceError); ok && destination != nil {
		native, found := errors.AsType[interface {
			error
			common.ServiceError
		}](err)
		if found {
			*destination = native
		}
		return found
	}
	return false
}

// ErrorCode maps native OCI failures to portable Go Cloud categories.
func ErrorCode(err error) gcerrors.ErrorCode {
	if errors.Is(err, context.Canceled) {
		return gcerrors.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return gcerrors.DeadlineExceeded
	}
	if errors.Is(err, gcerrors.ErrUnimplemented) {
		return gcerrors.Unimplemented
	}
	var native common.ServiceError
	if !ErrorAs(err, &native) {
		return gcerrors.Unknown
	}
	switch native.GetHTTPStatusCode() {
	case http.StatusBadRequest:
		return gcerrors.InvalidArgument
	case http.StatusUnauthorized, http.StatusForbidden:
		return gcerrors.PermissionDenied
	case http.StatusNotFound:
		return gcerrors.NotFound
	case http.StatusConflict:
		return gcerrors.AlreadyExists
	case http.StatusPreconditionFailed:
		return gcerrors.FailedPrecondition
	case http.StatusTooManyRequests:
		return gcerrors.ResourceExhausted
	case http.StatusNotImplemented:
		return gcerrors.Unimplemented
	default:
		return gcerrors.Unknown
	}
}
