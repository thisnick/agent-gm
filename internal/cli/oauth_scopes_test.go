package cli_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOAuthAdminScopeFlagsAreJSONArrays(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		field string
		want  []string
	}{
		{[]string{"admin", "enrollment-codes", "create", "test", "--scopes", "messages:read"}, "scopes", []string{"messages:read"}},
		{[]string{"admin", "enrollment-codes", "create", "test", "--scopes", "messages:read,messages:write"}, "scopes", []string{"messages:read", "messages:write"}},
		{[]string{"admin", "enrollment-codes", "create", "test", "--allow-scopes", "messages:delete"}, "allow_scopes", []string{"messages:delete"}},
		{[]string{"admin", "authorization-requests", "approve", "authreq_test", "--scopes", "messages:read messages:write"}, "scopes", []string{"messages:read", "messages:write"}},
	} {
		t.Run(tc.field+tc.args[2]+tc.args[len(tc.args)-1], func(t *testing.T) {
			s := newStub(t)
			got := runCLI(t, s, nil, "", tc.args...)
			if got.code != 0 {
				t.Fatalf("CLI exited %d: %s", got.code, got.stderr)
			}
			requests := s.seen()
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(requests[len(requests)-1].Body), &body); err != nil {
				t.Fatal(err)
			}
			var scopes []string
			if err := json.Unmarshal(body[tc.field], &scopes); err != nil {
				t.Fatalf("%s must be an array: %s", tc.field, body[tc.field])
			}
			if !reflect.DeepEqual(scopes, tc.want) {
				t.Fatalf("scopes = %v, want %v", scopes, tc.want)
			}
		})
	}
}
