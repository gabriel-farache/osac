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

import "log/slog"

// M360Builder builds the Adapter backed by the M360 RevOS API.
type M360Builder struct {
	cfg    Config
	logger *slog.Logger
}

func NewM360(cfg Config) *M360Builder {
	return &M360Builder{cfg: cfg}
}

func (b *M360Builder) SetLogger(value *slog.Logger) *M360Builder {
	b.logger = value
	return b
}

// Build fails with a NOT_CONFIGURED *Error when the configuration is incomplete.
func (b *M360Builder) Build() (Adapter, error) {
	if err := b.cfg.validate(); err != nil {
		return nil, err
	}
	logger := b.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &m360{client: newM360Client(b.cfg, logger)}, nil
}

type m360 struct {
	client *m360Client
}
