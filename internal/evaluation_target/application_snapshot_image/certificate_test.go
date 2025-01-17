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

package application_snapshot_image

import (
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"testing"

	"github.com/gkampitakis/go-snaps/snaps"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed other_name_san.cer
var otherNameSAN []byte

//go:embed chainguard_release.cer
var cosignReleaseCert []byte

func TestAddCertificateMetadata(t *testing.T) {
	cases := []struct {
		name string
		cert []byte
	}{
		{name: "Chainguard Cosign release", cert: cosignReleaseCert},
		{name: "OtherName SAN", cert: otherNameSAN},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {

			p, _ := pem.Decode(c.cert)
			cer, err := x509.ParseCertificate(p.Bytes)
			require.NoError(t, err)

			metadata := map[string]string{}
			err = addCertificateMetadataTo(&metadata, cer)
			assert.NoError(t, err)

			snaps.MatchSnapshot(t, metadata)

		})
	}
}
