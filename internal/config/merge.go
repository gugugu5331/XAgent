package config

import (
	"errors"
	"reflect"
	"strings"
)

type ConfigSource string

const (
	SourceDefault ConfigSource = "default"
	SourceUser    ConfigSource = "user"
	SourceProject ConfigSource = "project"
	SourceRuntime ConfigSource = "runtime"
)

type ConfigLayer struct {
	Source ConfigSource
	Path   string
	Value  PartialAppConfig
}

type MergeResult struct {
	Value      PartialAppConfig
	Provenance map[string]ConfigSource
}

// MergeLayers applies the fixed default < user < project < runtime priority.
// Call order cannot change precedence.
func MergeLayers(layers ...ConfigLayer) (MergeResult, error) {
	ordered := make([]*ConfigLayer, 4)
	for index := range layers {
		rank, ok := configSourceRank(layers[index].Source)
		if !ok {
			return MergeResult{}, errors.New("config layer has an unknown source")
		}
		if ordered[rank] != nil {
			return MergeResult{}, errors.New("config layers contain a duplicate source")
		}
		ordered[rank] = &layers[index]
	}

	result := MergeResult{Provenance: make(map[string]ConfigSource)}
	for _, layer := range ordered {
		if layer == nil {
			continue
		}
		mergePartialValue(
			reflect.ValueOf(&result.Value).Elem(),
			reflect.ValueOf(layer.Value),
			"",
			layer.Source,
			result.Provenance,
		)
	}
	return result, nil
}

func configSourceRank(source ConfigSource) (int, bool) {
	switch source {
	case SourceDefault:
		return 0, true
	case SourceUser:
		return 1, true
	case SourceProject:
		return 2, true
	case SourceRuntime:
		return 3, true
	default:
		return 0, false
	}
}

func mergePartialValue(target reflect.Value, incoming reflect.Value, path string, source ConfigSource, provenance map[string]ConfigSource) {
	if _, ok := optionalTarget(incoming); ok {
		if !incoming.FieldByName("Set").Bool() {
			return
		}
		target.Set(cloneConfigValue(incoming))
		provenance[path] = source
		return
	}

	switch incoming.Kind() {
	case reflect.Struct:
		for index := 0; index < incoming.NumField(); index++ {
			field := incoming.Type().Field(index)
			name := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			mergePartialValue(target.Field(index), incoming.Field(index), joinConfigPath(path, name), source, provenance)
		}
	case reflect.Map:
		mergeNamedConfigMap(target, incoming, path, source, provenance)
	}
}

func mergeNamedConfigMap(target reflect.Value, incoming reflect.Value, path string, source ConfigSource, provenance map[string]ConfigSource) {
	if incoming.IsNil() {
		return
	}
	if target.IsNil() {
		target.Set(reflect.MakeMapWithSize(target.Type(), incoming.Len()))
	}
	iterator := incoming.MapRange()
	for iterator.Next() {
		key := iterator.Key()
		itemPath := joinConfigPath(path, key.String())
		removeProvenancePrefix(provenance, itemPath)
		item := cloneConfigValue(iterator.Value())
		target.SetMapIndex(key, item)
		provenance[itemPath] = source
		recordConfigProvenance(item, itemPath, source, provenance)
	}
}

func recordConfigProvenance(value reflect.Value, path string, source ConfigSource, provenance map[string]ConfigSource) {
	if _, ok := optionalTarget(value); ok {
		if value.FieldByName("Set").Bool() {
			provenance[path] = source
		}
		return
	}
	if value.Kind() != reflect.Struct {
		return
	}
	for index := 0; index < value.NumField(); index++ {
		field := value.Type().Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		recordConfigProvenance(value.Field(index), joinConfigPath(path, name), source, provenance)
	}
}

func removeProvenancePrefix(provenance map[string]ConfigSource, path string) {
	delete(provenance, path)
	prefix := path + "."
	for existing := range provenance {
		if strings.HasPrefix(existing, prefix) {
			delete(provenance, existing)
		}
	}
}

func cloneConfigValue(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.NumField(); index++ {
			result.Field(index).Set(cloneConfigValue(value.Field(index)))
		}
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneConfigValue(iterator.Key()), cloneConfigValue(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneConfigValue(value.Index(index)))
		}
		return result
	default:
		return value
	}
}
