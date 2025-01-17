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

// Package image handles image operations, like creating a random image, image signature
// or attestation images
package image

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/cucumber/godog"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	s "github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/in-toto/in-toto-golang/in_toto"
	"github.com/jstemmer/go-junit-report/v2/junit"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/cosign/v2/pkg/oci"
	"github.com/sigstore/cosign/v2/pkg/oci/layout"
	cosignRemote "github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/cosign/v2/pkg/oci/static"
	cosigntypes "github.com/sigstore/cosign/v2/pkg/types"
	"github.com/sigstore/sigstore/pkg/signature"
	"gopkg.in/square/go-jose.v2/json"

	"github.com/enterprise-contract/ec-cli/acceptance/attestation"
	"github.com/enterprise-contract/ec-cli/acceptance/crypto"
	"github.com/enterprise-contract/ec-cli/acceptance/registry"
	"github.com/enterprise-contract/ec-cli/acceptance/testenv"
)

type key int

const (
	imageStateKey key = iota // Key to imageState struct within context
)

const (
	unknownConfig       = "application/vnd.unknown.config.v1+json"
	openPolicyAgentData = "application/vnd.cncf.openpolicyagent.data.layer.v1+json"
	title               = "org.opencontainers.image.title"
)

// Signature is the information about the signature of the image
type Signature struct {
	KeyID       string            `json:"keyid"`
	Signature   string            `json:"sig"`
	Certificate string            `json:"certificate,omitempty"`
	Chain       []string          `json:"chain,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// imageState holds the state of images used in acceptance tests keyed by the
// short image name, a name without the registry or the digest, e.g.
// "acceptance/hello-world". string values hold the concrete references, e.g.
// "registry:port/acceptance/sha256-hash.att" and the Signature values hold more
// information about the signature of the image/data itself.
type imageState struct {
	AttestationSignatures map[string]Signature
	Attestations          map[string]string
	Images                map[string]string
	ImageSignatures       map[string]Signature
	Signatures            map[string]string
}

func (i *imageState) Initialize() {
	if i.AttestationSignatures == nil {
		i.AttestationSignatures = map[string]Signature{}
	}
	if i.Attestations == nil {
		i.Attestations = map[string]string{}
	}
	if i.Images == nil {
		i.Images = map[string]string{}
	}
	if i.ImageSignatures == nil {
		i.ImageSignatures = map[string]Signature{}
	}
	if i.Signatures == nil {
		i.Signatures = map[string]string{}
	}
}

func (i imageState) Key() any {
	return imageStateKey
}

// imageFrom returns the named image from the Context
func imageFrom(ctx context.Context, imageName string) (v1.Image, error) {
	state := testenv.FetchState[imageState](ctx)

	if state.Images[imageName] == "" {
		return nil, fmt.Errorf("can't find image info for image named %s, did you create the image beforehand", imageName)
	}

	ref, err := name.ParseReference(state.Images[imageName])
	if err != nil {
		return nil, err
	}

	return remote.Image(ref)
}

// createAndPushImageSignature for a named image in the Context creates a signature
// image, same as `cosign sign` or Tekton Chains would, of that named image and pushes it
// to the stub registry as a new tag for that image akin to how cosign and Tekton Chains
// do it
func createAndPushImageSignature(ctx context.Context, imageName string, keyName string) (context.Context, error) {
	var state *imageState
	ctx, err := testenv.SetupState(ctx, &state)
	if err != nil {
		return ctx, err
	}

	if _, ok := state.Signatures[imageName]; ok {
		// we already created the signature
		return ctx, nil
	}

	image, err := imageFrom(ctx, imageName)
	if err != nil {
		return ctx, err
	}

	digest, err := image.Digest()
	if err != nil {
		return ctx, err
	}

	// the name of the image to sign referenced by the digest
	digestImage, err := name.NewDigest(fmt.Sprintf("%s@%s", imageName, digest.String()))
	if err != nil {
		return ctx, err
	}

	signer, err := crypto.SignerWithKey(ctx, keyName)
	if err != nil {
		return ctx, err
	}

	// creates a cosign signature payload signs it and provides the raw signature
	payload, signature, err := signature.SignImage(signer, digestImage, map[string]interface{}{})
	if err != nil {
		return ctx, err
	}

	signatureBase64 := base64.StdEncoding.EncodeToString(signature)

	// creates the layer with the image signature
	signatureLayer, err := static.NewSignature(payload, signatureBase64)
	if err != nil {
		return ctx, err
	}

	// creates the signature image with the correct media type and config and appends
	// the signature layer to it
	singnatureImage := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	singnatureImage = mutate.ConfigMediaType(singnatureImage, types.OCIConfigJSON)
	singnatureImage, err = mutate.Append(singnatureImage, mutate.Addendum{
		Layer: signatureLayer,
		Annotations: map[string]string{
			static.SignatureAnnotationKey: signatureBase64,
		},
	})
	if err != nil {
		return ctx, err
	}

	// the name of the image + the <hash>.sig tag
	ref, err := registry.ImageReferenceInStubRegistry(ctx, imageName+":%s-%s.sig", digest.Algorithm, digest.Hex)
	if err != nil {
		return ctx, err
	}

	// push to the registry
	err = remote.Write(ref, singnatureImage)
	if err != nil {
		return ctx, err
	}

	state.Signatures[imageName] = ref.String()
	state.ImageSignatures[imageName] = Signature{
		KeyID:     "",
		Signature: signatureBase64,
	}

	return ctx, nil
}

// createAndPushAttestation for a named image in the Context creates an attestation
// image, same as `cosign attest` or Tekton Chains would, and pushes it to the stub
// registry as a new tag for that image akin to how cosign and Tekton Chains do it
func createAndPushAttestation(ctx context.Context, imageName, keyName string) (context.Context, error) {
	return createAndPushAttestationWithPatches(ctx, imageName, keyName, nil)
}

// createAndPushAttestation for a named image in the Context creates an attestation
// image, same as `cosign attest` or Tekton Chains would, and pushes it to the stub
// registry as a new tag for that image akin to how cosign and Tekton Chains do
// it; this variant applies additional JSON Patch patches to the SLSA provenance
// statement as required by the tests
func createAndPushAttestationWithPatches(ctx context.Context, imageName, keyName string, patches *godog.Table) (context.Context, error) {
	var state *imageState
	ctx, err := testenv.SetupState(ctx, &state)
	if err != nil {
		return ctx, err
	}

	if state.Attestations[imageName] != "" {
		// we already created the attestation
		return ctx, nil
	}

	image, err := imageFrom(ctx, imageName)
	if err != nil {
		return ctx, err
	}

	// generates a mostly-empty statement, but with the required fields already filled in
	// at this point we could add more data to the statement but the minimum works, we'll
	// need to add more data to the attestation in more elaborate tests so:
	// TODO: create a hook to add more data to the attestation
	statement, err := attestation.CreateStatementFor(imageName, image)
	if err != nil {
		return ctx, err
	}

	statement, err = applyPatches(statement, patches)
	if err != nil {
		return ctx, err
	}

	// signs the attestation with the named key
	signedAttestation, err := attestation.SignStatement(ctx, keyName, *statement)
	if err != nil {
		return ctx, err
	}
	signatureBase64 := base64.StdEncoding.EncodeToString(signedAttestation)

	if sig, err := unmarshallSignatures(signedAttestation); err != nil {
		return ctx, err
	} else {
		state.AttestationSignatures[imageName] = Signature{
			KeyID:     sig.KeyID,
			Signature: sig.Sig,
		}
	}

	attestationLayer, err := static.NewAttestation(signedAttestation)
	if err != nil {
		return ctx, err
	}

	// creates the attestation image with the correct media type and config and appends
	// the attestation layer to it
	attestationImage := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	attestationImage = mutate.ConfigMediaType(attestationImage, types.OCIConfigJSON)
	attestationImage, err = mutate.Append(attestationImage, mutate.Addendum{
		MediaType: cosigntypes.DssePayloadType,
		Layer:     attestationLayer,
		Annotations: map[string]string{
			static.SignatureAnnotationKey: signatureBase64,
		},
	})
	if err != nil {
		return ctx, err
	}

	digest, err := image.Digest()
	if err != nil {
		return ctx, err
	}

	// the name of the image + the <hash>.att tag
	ref, err := registry.ImageReferenceInStubRegistry(ctx, imageName+":%s-%s.att", digest.Algorithm, digest.Hex)
	if err != nil {
		return ctx, err
	}

	// push to the registry
	err = remote.Write(ref, attestationImage)
	if err != nil {
		return ctx, err
	}

	if state.Attestations == nil {
		state.Attestations = make(map[string]string)
	}

	state.Attestations[imageName] = ref.String()

	return ctx, nil
}

// createAndPushImage creates a small 4K random image with 2 layers and pushes it to
// the stub image registry
func createAndPushImage(ctx context.Context, imageName string) (context.Context, error) {
	var state *imageState
	ctx, err := testenv.SetupState(ctx, &state)
	if err != nil {
		return ctx, err
	}

	if state.Attestations[imageName] != "" {
		// we already created the image
		return ctx, nil
	}

	img, err := random.Image(4096, 2)
	if err != nil {
		return ctx, err
	}

	ref, err := registry.ImageReferenceInStubRegistry(ctx, imageName)
	if err != nil {
		return ctx, err
	}

	// push to the registry
	err = remote.Write(ref, img)
	if err != nil {
		return ctx, err
	}

	if state.Images == nil {
		state.Images = make(map[string]string)
	}

	state.Images[imageName] = ref.String()

	return ctx, nil
}

// createAndPushKeylessImage loads an existing image from disk, along its signature and attestation
// into the docker registry.
func createAndPushKeylessImage(ctx context.Context, imageName string) (context.Context, error) {
	ref, err := registry.ImageReferenceInStubRegistry(ctx, imageName)
	if err != nil {
		return ctx, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return ctx, err
	}

	imageDir := path.Join(cwd, "acceptance", "image", "testimage")
	// get the signed image from disk
	sii, err := layout.SignedImageIndex(imageDir)
	if err != nil {
		return ctx, err
	}

	if err := cosignRemote.WriteSignedImageIndexImages(ref, sii); err != nil {
		return ctx, err
	}

	var state *imageState
	ctx, err = testenv.SetupState(ctx, &state)
	if err != nil {
		return ctx, err
	}

	if state.Images == nil {
		state.Images = make(map[string]string)
	}
	state.Images[imageName] = ref.String()

	storeSignatureData := func(where map[string]Signature, f func() (oci.Signatures, error)) error {
		sigs, err := f()
		if err != nil {
			return err
		}

		m, err := sigs.Manifest()
		if err != nil {
			return err
		}

		// read the signature from the layer annotations
		for _, l := range m.Layers {
			// just check for a single annotation
			var signature string
			var ok bool
			if signature, ok = l.Annotations[static.SignatureAnnotationKey]; !ok {
				continue
			}

			sig := Signature{
				Signature: signature,
			}

			if certPEM, ok := l.Annotations[static.CertificateAnnotationKey]; ok {
				certDER, _ := pem.Decode([]byte(certPEM))

				cert, err := x509.ParseCertificate(certDER.Bytes)
				if err != nil {
					return err
				}

				sig.KeyID = hex.EncodeToString(cert.SubjectKeyId)
				sig.Certificate = certPEM
			}

			if cert, ok := l.Annotations[static.ChainAnnotationKey]; ok {
				if strings.Contains(cert, "-\n-") {
					return errors.New("thus far we have only seen chain of length 1, fix the test to support more than one certificate in chain")
				}
				if !strings.HasSuffix(cert, "\n") { // for whatever reason the trailing newline is missing in the annotation
					cert += "\n"
				}
				sig.Chain = []string{cert} // TODO hmm
			}

			where[imageName] = sig

			return nil // TODO: support more than one signature

		}

		layers, err := sigs.Layers()
		if err != nil {
			return err
		}

		// read the signature from the layer content
		for _, l := range layers {
			r, err := l.Uncompressed()
			if err != nil {
				return err
			}

			raw, err := io.ReadAll(r)
			if err != nil {
				return err
			}

			sig, err := unmarshallSignatures(raw)
			if err != nil {
				return err
			}

			if sig != nil {
				where[imageName] = Signature{
					KeyID:     sig.KeyID,
					Signature: sig.Sig,
				}

				return nil // TODO: support more than one signature
			}
		}

		return nil
	}

	if sig, err := cosignRemote.SignatureTag(ref); err != nil {
		return ctx, err
	} else {
		state.Signatures[imageName] = sig.String()
	}

	if err := storeSignatureData(state.ImageSignatures, sii.Signatures); err != nil {
		return ctx, err
	}

	if att, err := cosignRemote.AttestationTag(ref); err != nil {
		return ctx, err
	} else {
		state.Attestations[imageName] = att.String()
	}

	if err := storeSignatureData(state.AttestationSignatures, sii.Attestations); err != nil {
		return ctx, err
	}

	return ctx, nil
}

// createAndPushPolicyBundle creates a OCI policy bundle with the given files as
// layers
func createAndPushPolicyBundle(ctx context.Context, imageName string, files *godog.Table) (context.Context, error) {
	bundle := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	bundle = mutate.ConfigMediaType(bundle, unknownConfig)

	// add each row as a layer to the bundle
	for _, row := range files.Rows {
		file := row.Cells[0].Value

		source := row.Cells[1].Value

		bytes, err := os.ReadFile(path.Join("acceptance", source))
		if err != nil {
			return ctx, err
		}

		if bundle, err = mutate.Append(bundle, mutate.Addendum{
			MediaType: openPolicyAgentData,
			Layer:     s.NewLayer(bytes, openPolicyAgentData),
			Annotations: map[string]string{
				title: file,
			},
		}); err != nil {
			return ctx, err
		}
	}

	ref, err := registry.ImageReferenceInStubRegistry(ctx, imageName)
	if err != nil {
		return ctx, err
	}

	// push to the registry
	err = remote.Write(ref, bundle)
	if err != nil {
		return ctx, err
	}

	return ctx, nil
}

// AttestationFrom finds the raw attestation created by the createAndPushAttestation
func AttestationFrom(ctx context.Context, imageName string) ([]byte, error) {
	state := testenv.FetchState[imageState](ctx)

	refStr := state.Attestations[imageName]

	if refStr == "" {
		return nil, fmt.Errorf("no attestation found for image %s, did you create a attestation beforehand", imageName)
	}

	ref, err := name.ParseReference(refStr)
	if err != nil {
		return nil, err
	}

	image, err := remote.Image(ref)
	if err != nil {
		return nil, err
	}

	layers, err := image.Layers()
	if err != nil {
		return nil, err
	}

	for _, layer := range layers {
		if mediaType, err := layer.MediaType(); err != nil {
			return nil, err
		} else if mediaType == cosigntypes.DssePayloadType {
			blob, err := layer.Uncompressed()
			if err != nil {
				return nil, err
			}
			defer blob.Close()

			return io.ReadAll(blob)
		}
	}

	return nil, fmt.Errorf("no attestation found for image %s, did you create a attestation beforehand", imageName)
}

// ImageSignatureFrom returns the image signature previously created by createAndPushImageSignature
func ImageSignatureFrom(ctx context.Context, imageName string) ([]byte, error) {
	state := testenv.FetchState[imageState](ctx)

	if sig, ok := state.ImageSignatures[imageName]; !ok {
		return nil, fmt.Errorf("no image signature found for image %s, did you create it beforehand?", imageName)
	} else {
		return json.Marshal(sig)
	}
}

// unmarshallSignatures extracts the signatures from the raw attestation
func unmarshallSignatures(rawCosignSignature []byte) (*cosign.Signatures, error) {
	var attestationPayload cosign.AttestationPayload
	if err := json.Unmarshal(rawCosignSignature, &attestationPayload); err != nil {
		return nil, err
	}

	l := len(attestationPayload.Signatures)
	switch {
	case l == 0:
		return nil, nil
	case l > 1:
		return nil, fmt.Errorf("received %d signatures, not expecting more than 1", l)
	default:
		return &attestationPayload.Signatures[0], nil
	}
}

// JSONAttestationSignaturesFrom returns the list of attestation signatures found in the context in
// JSON format. If not found, and empty JSON array is returned.
func JSONAttestationSignaturesFrom(ctx context.Context) (map[string]string, error) {
	if !testenv.HasState[imageState](ctx) {
		return nil, nil
	}

	state := testenv.FetchState[imageState](ctx)

	signatures := map[string]string{}
	for name, signature := range state.AttestationSignatures {
		json, err := json.Marshal(signature)
		if err != nil {
			return nil, err
		}
		signatures[name] = string(json)
	}

	return signatures, nil
}

func XMLAttestationSignaturesFrom(ctx context.Context) (map[string]string, error) {
	if !testenv.HasState[imageState](ctx) {
		return nil, nil
	}

	state := testenv.FetchState[imageState](ctx)

	type property struct {
		XMLName xml.Name `xml:"property"`
		junit.Property
	}

	ret := map[string]string{}
	for name, signature := range state.AttestationSignatures {
		properties := []property{
			{
				Property: junit.Property{
					Name:  "keyId",
					Value: signature.KeyID,
				},
			},
			{
				Property: junit.Property{
					Name:  "signature",
					Value: signature.Signature,
				},
			},
		}

		xml, err := xml.Marshal(properties)
		if err != nil {
			return nil, err
		}
		ret[name] = string(xml)
	}

	return ret, nil
}

func RawAttestationSignaturesFrom(ctx context.Context) map[string]string {
	if !testenv.HasState[imageState](ctx) {
		return nil
	}

	state := testenv.FetchState[imageState](ctx)

	ret := map[string]string{}
	for ref, signature := range state.AttestationSignatures {
		ret[fmt.Sprintf("ATTESTATION_SIGNATURE_%s", ref)] = signature.Signature
	}

	return ret
}

func JSONImageSignaturesFrom(ctx context.Context) (map[string]string, error) {
	if !testenv.HasState[imageState](ctx) {
		return nil, nil
	}

	state := testenv.FetchState[imageState](ctx)

	ret := map[string]string{}
	for name, signature := range state.ImageSignatures {
		json, err := json.Marshal(signature)
		if err != nil {
			return nil, err
		}
		ret[name] = string(json)
	}

	return ret, nil
}

func XMLImageSignaturesFrom(ctx context.Context) (map[string]string, error) {
	if !testenv.HasState[imageState](ctx) {
		return nil, nil
	}

	state := testenv.FetchState[imageState](ctx)

	type property struct {
		XMLName xml.Name `xml:"property"`
		junit.Property
	}

	ret := map[string]string{}
	for name, signature := range state.ImageSignatures {
		properties := []property{
			{
				Property: junit.Property{
					Name:  "keyId",
					Value: signature.KeyID,
				},
			},
			{
				Property: junit.Property{
					Name:  "signature",
					Value: signature.Signature,
				},
			},
		}

		xml, err := xml.Marshal(properties)
		if err != nil {
			return nil, err
		}
		ret[name] = string(xml)
	}

	return ret, nil
}

func RawImageSignaturesFrom(ctx context.Context) map[string]string {
	if !testenv.HasState[imageState](ctx) {
		return nil
	}

	state := testenv.FetchState[imageState](ctx)

	ret := map[string]string{}
	for ref, signature := range state.ImageSignatures {
		ret[fmt.Sprintf("IMAGE_SIGNATURE_%s", ref)] = signature.Signature
	}

	return ret
}

func applyPatches(statement *in_toto.ProvenanceStatement, patches *godog.Table) (*in_toto.ProvenanceStatement, error) {
	if statement == nil || patches == nil || len(patches.Rows) == 0 {
		return statement, nil
	}

	stmt, err := json.Marshal(statement)
	if err != nil {
		return nil, err
	}

	for _, patch := range patches.Rows {
		val := patch.Cells[0].Value
		jp, err := jsonpatch.DecodePatch([]byte(val))
		if err != nil {
			return nil, err
		}

		stmt, err = jp.Apply(stmt)
		if err != nil {
			return nil, err
		}
	}

	var modified in_toto.ProvenanceStatement
	json.Unmarshal(stmt, &modified)

	return &modified, nil
}

// steal creates an image using createAndPushImage and steals the signature
// ("sig") or attestation ("att")
func steal(what string) func(context.Context, string, string) (context.Context, error) {
	return func(ctx context.Context, imageName string, signatureFrom string) (context.Context, error) {
		ctx, err := createAndPushImage(ctx, imageName)
		if err != nil {
			return ctx, err
		}

		fromImg, err := imageFrom(ctx, signatureFrom)
		if err != nil {
			return ctx, err
		}

		fromDigest, err := fromImg.Digest()
		if err != nil {
			return ctx, err
		}

		fromRef, err := registry.ImageReferenceInStubRegistry(ctx, signatureFrom+":%s-%s.%s", fromDigest.Algorithm, fromDigest.Hex, what)
		if err != nil {
			return ctx, err
		}

		stolen, err := remote.Image(fromRef)
		if err != nil {
			return ctx, err
		}

		toImg, err := imageFrom(ctx, imageName)
		if err != nil {
			return ctx, err
		}

		toDigest, err := toImg.Digest()
		if err != nil {
			return ctx, err
		}

		toRef, err := registry.ImageReferenceInStubRegistry(ctx, imageName+":%s-%s.%s", toDigest.Algorithm, toDigest.Hex, what)
		if err != nil {
			return ctx, err
		}

		return ctx, remote.Write(toRef, stolen)
	}
}

func copyAllImages(ctx context.Context, source, destination string) (context.Context, error) {
	state := testenv.FetchState[imageState](ctx)

	pusher, err := remote.NewPusher()
	if err != nil {
		return ctx, err
	}

	for _, imgs := range []map[string]string{state.Images, state.Signatures, state.Attestations} {
		var sourceStr string
		var ok bool
		if sourceStr, ok = imgs[source]; !ok {
			continue
		}

		sourceRef, err := name.ParseReference(sourceStr)
		if err != nil {
			return ctx, err
		}

		destStr := strings.ReplaceAll(sourceStr, source, destination)
		destRef, err := name.ParseReference(destStr)
		if err != nil {
			return ctx, err
		}

		desc, err := remote.Get(sourceRef)
		if err != nil {
			return ctx, err
		}

		if err := pusher.Push(ctx, destRef, desc); err != nil {
			return ctx, err
		}

		imgs[destination] = destStr
	}

	return ctx, nil
}

// AddStepsTo adds Gherkin steps to the godog ScenarioContext
func AddStepsTo(sc *godog.ScenarioContext) {
	sc.Step(`^an image named "([^"]*)"$`, createAndPushImage)
	sc.Step(`^a valid image signature of "([^"]*)" image signed by the "([^"]*)" key$`, createAndPushImageSignature)
	sc.Step(`^a valid attestation of "([^"]*)" signed by the "([^"]*)" key$`, createAndPushAttestation)
	sc.Step(`^a valid attestation of "([^"]*)" signed by the "([^"]*)" key, patched with$`, createAndPushAttestationWithPatches)
	sc.Step(`^a signed and attested keyless image named "([^"]*)"$`, createAndPushKeylessImage)
	sc.Step(`^a OCI policy bundle named "([^"]*)" with$`, createAndPushPolicyBundle)
	sc.Step(`^an image named "([^"]*)" with signature from "([^"]*)"$`, steal("sig"))
	sc.Step(`^an image named "([^"]*)" with attestation from "([^"]*)"$`, steal("att"))
	sc.Step(`^all images relating to "([^"]*)" are copied to "([^"]*)"$`, copyAllImages)
}
