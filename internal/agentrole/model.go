package agentrole

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

type ErrorCode string

const (
	ErrProviderLimit         ErrorCode = "role_provider_limit"
	ErrProviderFailed        ErrorCode = "role_provider_failed"
	ErrModelAliasUnavailable ErrorCode = "role_model_alias_unavailable"
)

type ModelValidator func(string) error

type ModelResolutionError struct {
	Alias  ModelAlias
	Reason ErrorCode
}

func (e *ModelResolutionError) Error() string {
	if e == nil || e.Reason == "" {
		return string(ErrModelAliasUnavailable)
	}
	return string(e.Reason)
}

type ModelCatalogOptions struct {
	ProviderID    string
	DefaultModel  string
	Aliases       ModelAliases
	MaxModelBytes int64
	Validate      ModelValidator
}

type ModelCatalog struct {
	Default    string
	providerID string
	models     map[ModelAlias]string
	metadata   []ModelMetadata
	maxBytes   int64
	validate   ModelValidator
}

func NewModelCatalog(options ModelCatalogOptions) (ModelCatalog, error) {
	if options.MaxModelBytes <= 0 || options.Validate == nil {
		return ModelCatalog{}, errors.New("invalid model catalog options")
	}
	providerID, err := normalizeIdentifier(options.ProviderID, options.MaxModelBytes)
	if err != nil {
		return ModelCatalog{}, errors.New("invalid model catalog provider")
	}
	if err := validateConcreteModel(options.DefaultModel, options.MaxModelBytes, options.Validate); err != nil {
		return ModelCatalog{}, errors.New("invalid default model")
	}

	mappings := map[ModelAlias]string{
		ModelHaiku:  options.Aliases.Haiku,
		ModelSonnet: options.Aliases.Sonnet,
		ModelOpus:   options.Aliases.Opus,
	}
	for alias, model := range mappings {
		if model == "" {
			continue
		}
		if err := validateConcreteModel(model, options.MaxModelBytes, options.Validate); err != nil {
			return ModelCatalog{}, errors.New("invalid model alias mapping")
		}
		mappings[alias] = model
	}

	metadata := []ModelMetadata{
		{Alias: ModelInherit, Concrete: options.DefaultModel, Provider: providerID, Available: true, Dynamic: true},
	}
	for _, alias := range []ModelAlias{ModelHaiku, ModelSonnet, ModelOpus} {
		model := mappings[alias]
		metadata = append(metadata, ModelMetadata{
			Alias:     alias,
			Concrete:  model,
			Provider:  providerID,
			Available: model != "",
		})
	}
	sort.Slice(metadata, func(i, j int) bool { return metadata[i].Alias < metadata[j].Alias })

	return ModelCatalog{
		Default:    options.DefaultModel,
		providerID: providerID,
		models:     mappings,
		metadata:   metadata,
		maxBytes:   options.MaxModelBytes,
		validate:   options.Validate,
	}, nil
}

func (c ModelCatalog) Resolve(alias ModelAlias, inheritedModel string) (string, error) {
	fail := func() (string, error) {
		return "", &ModelResolutionError{Alias: alias, Reason: ErrModelAliasUnavailable}
	}
	switch alias {
	case ModelInherit:
		model := inheritedModel
		if model == "" {
			model = c.Default
		}
		if err := validateConcreteModel(model, c.maxBytes, c.validate); err != nil {
			return fail()
		}
		return model, nil
	case ModelHaiku, ModelSonnet, ModelOpus:
		model := c.models[alias]
		if model == "" {
			return fail()
		}
		return model, nil
	default:
		return fail()
	}
}

func (c ModelCatalog) Models() []ModelMetadata {
	return append([]ModelMetadata(nil), c.metadata...)
}

func validateConcreteModel(model string, maxBytes int64, validate ModelValidator) error {
	if maxBytes <= 0 || validate == nil || model == "" || int64(len(model)) > maxBytes || strings.TrimSpace(model) != model {
		return errors.New("invalid model")
	}
	if strings.ContainsAny(model, "\x00\x1b\r\n\t") {
		return errors.New("invalid model")
	}
	return validate(model)
}

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func normalizeIdentifier(value string, maxBytes int64) (string, error) {
	trimmed := strings.TrimSpace(value)
	normalized := strings.ToLower(trimmed)
	if normalized == "" || int64(len(normalized)) > maxBytes || !identifierPattern.MatchString(normalized) {
		return "", errors.New("invalid identifier")
	}
	return normalized, nil
}
