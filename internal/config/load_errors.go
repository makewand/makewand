package config

import (
	"encoding/json"
	"errors"
)

// CredentialSourceWarning reports an unusable optional credential source.
// Explicit environment fields and valid config.json fields remain usable.
type CredentialSourceWarning struct{ Err error }

func (w *CredentialSourceWarning) Error() string { return w.Err.Error() }
func (w *CredentialSourceWarning) Unwrap() error { return w.Err }

// IsFatalLoadError distinguishes an invalid execution policy from an optional
// credentials warning. Callers must refuse execution for fatal load errors.
func IsFatalLoadError(err error) bool {
	var warning *CredentialSourceWarning
	return err != nil && !errors.As(err, &warning)
}

func validateJSONObject(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("expected a JSON object")
	}
	return nil
}

func invalidPolicyConfig(err error) (*Config, error) {
	cfg := DefaultConfig()
	cfg.policyLoadErr = err
	return cfg, err
}
