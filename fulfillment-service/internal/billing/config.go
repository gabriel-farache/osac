/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package billing

// Config is supplied by the caller; this package reads no flags, environment or files. Embedding opaque keeps the
// credential out of logs and formatted output.
type Config struct {
	opaque
	BaseURL    string
	APIVersion string
	Credential string
}

// Validate fails with a NOT_CONFIGURED *Error when the configuration is incomplete.
func (c Config) Validate() error {
	if c.BaseURL == "" || c.APIVersion == "" || c.Credential == "" {
		return &Error{Reason: ReasonNotConfigured, Op: "configure"}
	}
	return nil
}
