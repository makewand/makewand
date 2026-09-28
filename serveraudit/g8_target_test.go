package serveraudit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// go-server#11: admin audit events carry what was changed, not only the path.
func TestG8_EventTargetFieldsRoundTrip(t *testing.T) {
	active := false
	evt := Event{
		Timestamp:            time.Unix(1700000000, 0).UTC(),
		Kind:                 "admin_organization_memberships",
		ActorUserID:          "usr_actor",
		Action:               "upsert_organization_membership",
		TargetUserID:         "usr_bob",
		TargetOrganizationID: "org-a",
		TargetProjectID:      "project-a",
		TargetTokenID:        "tok_1",
		TargetRole:           "owner",
		TargetActive:         &active,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"actor_user_id":"usr_actor"`, `"action":"upsert_organization_membership"`, `"target_user_id":"usr_bob"`, `"target_organization_id":"org-a"`, `"target_project_id":"project-a"`, `"target_token_id":"tok_1"`, `"target_role":"owner"`, `"target_active":false`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("event JSON missing %s: %s", field, data)
		}
	}
	var buf bytes.Buffer
	if err := WriteEventsCSV(&buf, []Event{evt}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.HasSuffix(lines[0], "actor_user_id,action,target_user_id,target_organization_id,target_project_id,target_token_id,target_role,target_active") {
		t.Fatalf("CSV header=%s", lines[0])
	}
	if !strings.HasSuffix(lines[1], "usr_actor,upsert_organization_membership,usr_bob,org-a,project-a,tok_1,owner,false") {
		t.Fatalf("CSV row=%s", lines[1])
	}
}
