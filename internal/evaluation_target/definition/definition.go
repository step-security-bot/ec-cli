// Copyright 2022 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package definition

import (
	"context"
	"fmt"

	"github.com/spf13/afero"

	"github.com/hacbs-contract/ec-cli/internal/evaluator"
	"github.com/hacbs-contract/ec-cli/internal/policy"
	"github.com/hacbs-contract/ec-cli/internal/policy/source"
	"github.com/hacbs-contract/ec-cli/internal/utils"
)

var newConftestEvaluator = evaluator.NewConftestEvaluator
var pathExists = afero.Exists

// DefinitionFile represents the structure needed to evaluate a pipeline definition file
type Definition struct {
	Fpath     string
	Evaluator evaluator.Evaluator
}

// NewPipelineDefinitionFile returns a DefinitionFile struct with FPath and evaluator ready to use
func NewDefinition(ctx context.Context, fpath string, sources []source.PolicySource) (*Definition, error) {
	fs := utils.FS(ctx)
	exists, err := pathExists(fs, fpath)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("fpath '%s' does not exist", fpath)
	}
	p := &Definition{
		Fpath: fpath,
	}

	pol, err := policy.NewOfflinePolicy(ctx, policy.Now)
	if err != nil {
		return nil, err
	}

	c, err := newConftestEvaluator(ctx, sources, pol)
	if err != nil {
		return nil, err
	}
	p.Evaluator = c

	return p, nil
}