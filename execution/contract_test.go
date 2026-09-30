package execution

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalExecutionContract(t *testing.T) {
	data, err := os.ReadFile("../makewand/execution_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Schema      int                        `json:"schema"`
		StatusCodes map[Status]int             `json:"status_codes"`
		Fixtures    map[string]json.RawMessage `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	if contract.Schema != Schema || len(contract.StatusCodes) != 11 {
		t.Fatalf("unsupported contract: %+v", contract)
	}
	for status, code := range contract.StatusCodes {
		if status.ExitCode() != code {
			t.Errorf("%s: Go=%d canonical=%d", status, status.ExitCode(), code)
		}
	}
	for _, entry := range []struct {
		name  string
		value any
	}{{"request", new(Request)}, {"result", new(Result)}, {"event", new(Event)}} {
		t.Run(entry.name, func(t *testing.T) {
			fixture := contract.Fixtures[entry.name]
			if err := json.Unmarshal(fixture, entry.value); err != nil {
				t.Fatal(err)
			}
			if request, ok := entry.value.(*Request); ok {
				if err := request.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			if result, ok := entry.value.(*Result); ok {
				if err := result.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			if event, ok := entry.value.(*Event); ok {
				if err := event.Validate(); err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := json.Marshal(entry.value)
			if err != nil {
				t.Fatal(err)
			}
			var original, roundtrip any
			if err := json.Unmarshal(fixture, &original); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, roundtrip) {
				t.Fatalf("fixture changed:\n%s\n%s", fixture, encoded)
			}
		})
	}
}

func TestMalformedWireContractFailsClosed(t *testing.T) {
	base := `{"schema":1,"task_id":"task","stage":"provider","engine":"synthetic","tier":"standard","readonly":true,"repo_trust":"trusted","api_policy":"subscription_only"}`
	for _, body := range []string{
		strings.Replace(base, `"schema":1,`, "", 1),
		strings.Replace(base, `"schema":1`, `"schema":true`, 1),
		strings.Replace(base, `"readonly":true`, `"readonly":null`, 1),
		strings.TrimSuffix(base, "}") + `,"unrecognized":1}`,
		strings.TrimSuffix(base, "}") + `,"timeout_ms":true}`,
		strings.TrimSuffix(base, "}") + `,"workflow":"unrecognized"}`,
	} {
		if _, err := DecodeRequest(strings.NewReader(body)); err == nil {
			t.Fatalf("malformed request accepted: %s", body)
		}
	}
	if _, err := DecodeRequest(strings.NewReader(base)); err != nil {
		t.Fatal(err)
	}
	result := `{"schema":1,"task_id":"task","stage":"provider","engine":"synthetic","status":"UNKNOWN","exit_code":17,"readonly":true,"outcome_known":true}`
	if _, err := DecodeResult(strings.NewReader(result)); err == nil {
		t.Fatal("unknown result claimed known outcome")
	}
	status := Passed
	duration := int64(1)
	for _, event := range []Event{
		{Schema: 1, EventID: "span", TaskID: "task", Stage: "copy", Event: "start", Status: &status},
		{Schema: 1, EventID: "span", TaskID: "task", Stage: "copy", Event: "end", DurationMS: &duration},
		{Schema: 1, EventID: "span", TaskID: "task", Stage: "copy", Event: "end", Status: &status},
	} {
		if err := event.Validate(); err == nil {
			t.Fatalf("malformed event accepted: %+v", event)
		}
	}
}

func TestContractNullableValuesRoundTripAndInvalidStatus(t *testing.T) {
	request := Request{Schema: 1, TaskID: "task", Stage: "provider", Engine: "synthetic", Tier: "standard", RepoTrust: "trusted", APIPolicy: "subscription_only"}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"model", "account_ref", "deadline_unix_ms", "timeout_ms", "budget_file", "max_model_calls", "workflow", "risk", "prompt", "cwd"} {
		if value, exists := values[field]; !exists || value != nil {
			t.Errorf("%s not explicit null: %s", field, data)
		}
	}
	result := Result{Schema: 1, TaskID: "task", Stage: "provider", Engine: "synthetic", Status: "UNSUPPORTED", ExitCode: 1}
	if err := result.Validate(); err == nil {
		t.Fatal("unknown result status accepted")
	}
	event := Event{Schema: 1, EventID: "span", TaskID: "task", Stage: "prepare", Event: "start", StartUnixMS: 1}
	data, err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatal(err)
	}
	if values["engine"] != nil {
		t.Fatal("unknown stage engine guessed")
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}
