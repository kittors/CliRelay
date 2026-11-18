package config

import (
	"reflect"

	"gopkg.in/yaml.v3"
)

var yamlNodeType = reflect.TypeOf(yaml.Node{})

// CloneForRuntime returns an independent in-memory snapshot of the full config.
func (cfg *Config) CloneForRuntime() *Config {
	if cfg == nil {
		return nil
	}
	cloned := cloneRuntimeValue(reflect.ValueOf(cfg))
	out := cloned.Interface().(*Config)
	// runtimeSettings is intentionally shared across value/runtime clones:
	// management saves use it as the optimistic-concurrency snapshot for the
	// configuration row. It is unexported, so restore it explicitly after the
	// reflective copy skips unexported fields.
	out.runtimeSettings = cfg.runtimeSettings
	return out
}

func cloneRuntimeValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	if v.Type() == yamlNodeType {
		node := v.Interface().(yaml.Node)
		return reflect.ValueOf(*deepCopyNode(&node))
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(cloneRuntimeValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		return cloneRuntimeValue(v.Elem())
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.NumField(); i++ {
			dst := out.Field(i)
			if !dst.CanSet() {
				// Unexported runtime-only fields cannot be set through reflection.
				// They are intentionally left at their zero value in the clone;
				// persisted/configuration fields are exported and continue to be
				// copied recursively.
				continue
			}
			dst.Set(cloneRuntimeValue(v.Field(i)))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneRuntimeValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneRuntimeValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(cloneRuntimeValue(iter.Key()), cloneRuntimeValue(iter.Value()))
		}
		return out
	default:
		return v
	}
}
