package execution

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// DecodeRequest validates the JSON wire contract, including explicit schema,
// unknown fields and null/boolean distinctions that Go zero values cannot show.
func DecodeRequest(reader io.Reader) (Request, error) {
	var request Request
	if err := decodeWire(reader, &request, "readonly"); err != nil {
		return request, err
	}
	return request, request.Validate()
}

func DecodeResult(reader io.Reader) (Result, error) {
	var result Result
	if err := decodeWire(reader, &result, "readonly", "outcome_known"); err != nil {
		return result, err
	}
	return result, result.Validate()
}

func DecodeEvent(reader io.Reader) (Event, error) {
	var event Event
	if err := decodeWire(reader, &event, "readonly"); err != nil {
		return event, err
	}
	return event, event.Validate()
}

func decodeWire(reader io.Reader, value any, booleanFields ...string) error {
	data, err := io.ReadAll(io.LimitReader(reader, maximumLedgerBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maximumLedgerBytes {
		return fmt.Errorf("execution JSON exceeds 16 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if schema, exists := fields["schema"]; !exists || !bytes.Equal(bytes.TrimSpace(schema), []byte("1")) {
		return fmt.Errorf("execution JSON requires explicit schema 1")
	}
	for _, field := range booleanFields {
		if raw, exists := fields[field]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("true")) && !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
			return fmt.Errorf("execution %s must be boolean", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
