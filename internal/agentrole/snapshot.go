package agentrole

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"

	"xagent/internal/diagnostics"
)

type Snapshot struct {
	Generation         uint64
	Fingerprint        string
	Catalog            []CatalogItem
	Definitions        map[string]Definition
	Diagnostics        []diagnostics.SafeDiagnostic
	DiagnosticsDropped uint64
	Tools              []ToolMetadata
	Models             []ModelMetadata
}

type ResolvedRole struct {
	Generation uint64
	Definition Definition
}

type RefreshResult struct {
	Published          bool
	Changed            bool
	Generation         uint64
	Snapshot           Snapshot
	Diagnostics        []diagnostics.SafeDiagnostic
	DiagnosticsDropped uint64
	Error              *diagnostics.SafeError
}

func (m Metadata) clone() Metadata {
	result := m
	result.ToolAllow = cloneStringsPreserveNil(m.ToolAllow)
	result.ToolDeny = cloneStringsPreserveNil(m.ToolDeny)
	if m.MaxIterations != nil {
		value := *m.MaxIterations
		result.MaxIterations = &value
	}
	return result
}

func (d Definition) Clone() Definition {
	result := d
	result.Metadata = d.Metadata.clone()
	return result
}

func (c Candidate) clone() Candidate {
	result := c
	result.Metadata = c.Metadata.clone()
	result.Diagnostics = append([]diagnostics.SafeDiagnostic(nil), c.Diagnostics...)
	return result
}

func (s Snapshot) Clone() Snapshot {
	result := s
	result.Catalog = append([]CatalogItem(nil), s.Catalog...)
	if s.Definitions != nil {
		result.Definitions = make(map[string]Definition, len(s.Definitions))
		for name, definition := range s.Definitions {
			result.Definitions[name] = definition.Clone()
		}
	}
	result.Diagnostics = append([]diagnostics.SafeDiagnostic(nil), s.Diagnostics...)
	result.Tools = append([]ToolMetadata(nil), s.Tools...)
	result.Models = append([]ModelMetadata(nil), s.Models...)
	return result
}

func cloneStringsPreserveNil(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func definitionFingerprint(definition Definition) string {
	encoder := newCanonicalEncoder("xagent-role-definition-v1")
	encoder.add(string(definition.Source))
	encoder.add(definition.SourceID)
	encoder.add(definition.ProviderID)
	encoder.add(definition.Origin.Text())
	encodeMetadata(encoder, definition.Metadata)
	encoder.add(definition.Instructions.Text())
	return encoder.sum()
}

func snapshotFingerprint(snapshot Snapshot, candidateIdentities []string) string {
	encoder := newCanonicalEncoder("xagent-role-snapshot-v1")
	identities := append([]string(nil), candidateIdentities...)
	sort.Strings(identities)
	for _, identity := range identities {
		encoder.add(identity)
	}
	names := make([]string, 0, len(snapshot.Definitions))
	for name := range snapshot.Definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		definition := snapshot.Definitions[name]
		encoder.add(name)
		encoder.add(definition.Fingerprint)
	}
	for _, item := range snapshot.Catalog {
		encoder.add(item.Name)
		encoder.add(item.Description.Text())
		encoder.add(string(item.Source))
		encoder.add(item.SourceID)
		encoder.add(item.ProviderID)
	}
	for _, diagnostic := range snapshot.Diagnostics {
		encoder.add(diagnostic.Code)
		encoder.add(diagnostic.Source)
		encoder.add(diagnostic.Hint)
		encoder.add(string(diagnostic.Severity))
		encoder.add(diagnostic.Message.Text())
	}
	encoder.add(strconv.FormatUint(snapshot.DiagnosticsDropped, 10))
	for _, tool := range snapshot.Tools {
		encoder.add(tool.Name)
		encoder.add(strconv.FormatBool(tool.ReadOnly))
		encoder.add(strconv.FormatBool(tool.SideEffectFree))
		encoder.add(strconv.FormatBool(tool.ConcurrentSafe))
	}
	for _, model := range snapshot.Models {
		encoder.add(string(model.Alias))
		encoder.add(model.Concrete)
		encoder.add(model.Provider)
		encoder.add(strconv.FormatBool(model.Available))
		encoder.add(strconv.FormatBool(model.Dynamic))
	}
	return encoder.sum()
}

func candidateFingerprint(candidate Candidate, provenance Provenance) string {
	encoder := newCanonicalEncoder("xagent-role-candidate-v1")
	encoder.add(string(provenance.Source))
	encoder.add(provenance.SourceID)
	encoder.add(provenance.ProviderID)
	encoder.add(provenance.Origin.Text())
	encoder.add(strconv.FormatBool(candidate.Valid))
	encodeMetadata(encoder, candidate.Metadata)
	encoder.add(candidate.Instructions.Text())
	for _, diagnostic := range candidate.Diagnostics {
		encoder.add(diagnostic.Code)
		encoder.add(diagnostic.Source)
		encoder.add(diagnostic.Hint)
		encoder.add(string(diagnostic.Severity))
		encoder.add(diagnostic.Message.Text())
	}
	return encoder.sum()
}

func encodeMetadata(encoder *canonicalEncoder, metadata Metadata) {
	encoder.add(metadata.Name)
	encoder.add(metadata.Description.Text())
	if metadata.ToolAllow == nil {
		encoder.add("allow:nil")
	} else {
		encoder.add("allow:set")
		for _, name := range metadata.ToolAllow {
			encoder.add(name)
		}
	}
	if metadata.ToolDeny == nil {
		encoder.add("deny:nil")
	} else {
		encoder.add("deny:set")
		for _, name := range metadata.ToolDeny {
			encoder.add(name)
		}
	}
	encoder.add(string(metadata.Model))
	if metadata.MaxIterations == nil {
		encoder.add("iterations:nil")
	} else {
		encoder.add(strconv.Itoa(*metadata.MaxIterations))
	}
	encoder.add(string(metadata.PermissionMode))
	encoder.add(string(metadata.Isolation))
}

type canonicalEncoder struct {
	hash hash.Hash
}

func newCanonicalEncoder(version string) *canonicalEncoder {
	encoder := &canonicalEncoder{hash: sha256.New()}
	encoder.add(version)
	return encoder
}

func (e *canonicalEncoder) add(value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = e.hash.Write(size[:])
	_, _ = e.hash.Write([]byte(value))
}

func (e *canonicalEncoder) sum() string {
	return hex.EncodeToString(e.hash.Sum(nil))
}
