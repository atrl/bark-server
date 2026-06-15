package apns

import "testing"

func TestPushMessageIsDeleteAcceptsTruthyDeleteValues(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
	}{
		{name: "string one", value: "1"},
		{name: "integer one", value: 1},
		{name: "float one", value: 1.0},
		{name: "boolean true", value: true},
		{name: "string true", value: "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := PushMessage{ExtParams: map[string]interface{}{"delete": tt.value}}

			if !message.IsDelete() {
				t.Fatalf("delete=%#v should be treated as delete", tt.value)
			}
		})
	}
}
