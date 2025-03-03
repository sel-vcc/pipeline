/*
Copyright 2022 The Tekton Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1beta1

import (
	"context"
	"fmt"

	"knative.dev/pkg/apis"
)

// Validate implements apis.Validatable
func (tr TaskResult) Validate(_ context.Context) *apis.FieldError {
	if !resultNameFormatRegex.MatchString(tr.Name) {
		return apis.ErrInvalidKeyName(tr.Name, "name", fmt.Sprintf("Name must consist of alphanumeric characters, '-', '_', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my-name',  or 'my_name', regex used for validation is '%s')", ResultNameFormat))
	}
	// Validate the result type
	validType := false
	for _, allowedType := range AllResultsTypes {
		if tr.Type == allowedType {
			validType = true
		}
	}
	if !validType {
		return apis.ErrInvalidValue(tr.Type, "type", fmt.Sprintf("type must be string"))
	}

	return nil
}
