package i18n

import (
	"reflect"
	"testing"
)

// Every message must exist in both languages; a missing translation renders
// as an empty notice (for example a silently empty "discarded test edits"
// warning).
func TestAllMessagesTranslated(t *testing.T) {
	for name, msgs := range map[string]Messages{"en": en, "zh": zh} {
		v := reflect.ValueOf(msgs)
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.Type.Kind() == reflect.String && v.Field(i).String() == "" {
				t.Errorf("[%s] %s is empty", name, field.Name)
			}
		}
	}
}
