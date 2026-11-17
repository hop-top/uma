package postgres

import (
	"encoding/json"
	"reflect"

	"hop.top/uma/model"
)

// Record bodies are encoding/json documents of the record value under its
// explicit snake_case json keys, stored in json (not jsonb) columns so the
// bytes read back are the bytes written. Renaming a Go field never changes
// the stored shape; changing a key does, and needs a schema version.
//
// encoding/json cannot represent an empty json.RawMessage. Bodies therefore
// clear every empty raw message before encoding (omitempty then drops it)
// and read every literal null raw message back as nil: extension data is
// either absent or a JSON value other than null.

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// encode marshals v, which the caller owns and has already cloned, after
// clearing its empty raw messages.
func encode[T any](v *T) ([]byte, error) {
	walkRaw(reflect.ValueOf(v).Elem(), func(m json.RawMessage) json.RawMessage {
		if len(m) == 0 {
			return nil
		}
		return m
	})
	b, err := json.Marshal(v)
	if err != nil {
		return nil, invalid("record: cannot be encoded")
	}
	return b, nil
}

// decode unmarshals a stored body.
func decode[T any](b []byte) (T, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return v, model.NewError(model.Permanent, "stored record cannot be decoded")
	}
	walkRaw(reflect.ValueOf(&v).Elem(), func(m json.RawMessage) json.RawMessage {
		if string(m) == "null" {
			return nil
		}
		return m
	})
	return v, nil
}

// walkRaw replaces every json.RawMessage reachable from v through exported
// fields, pointers, slices, arrays and map values with fix(m).
func walkRaw(v reflect.Value, fix func(json.RawMessage) json.RawMessage) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walkRaw(v.Elem(), fix)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				walkRaw(v.Field(i), fix)
			}
		}
	case reflect.Slice:
		if v.Type() == rawMessageType {
			if v.CanSet() {
				v.Set(reflect.ValueOf(fix(v.Interface().(json.RawMessage))))
			}
			return
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := range v.Len() {
			walkRaw(v.Index(i), fix)
		}
	case reflect.Array:
		for i := range v.Len() {
			walkRaw(v.Index(i), fix)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			e := reflect.New(iter.Value().Type()).Elem()
			e.Set(iter.Value())
			walkRaw(e, fix)
			v.SetMapIndex(iter.Key(), e)
		}
	}
}

// extData returns extension data as a nullable json argument.
func extData(m json.RawMessage) []byte {
	if len(m) == 0 {
		return nil
	}
	return m
}
