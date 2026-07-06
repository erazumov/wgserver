package wg

import (
	"errors"
	"fmt"
)

var errEmptyPSK = errors.New("wg genpsk returned empty key")

// GeneratePresharedKey shells out to `wg genpsk` and returns a base64
// preshared key. `wg genpsk` writes the key to stdout (and is the
// standard, recommended way to generate a PSK), so unlike genkey we
// don't need a temp file to keep the secret off a command line.
func GeneratePresharedKey(r Runner) (string, error) {
	out, err := r.Output("wg", "genpsk")
	if err != nil {
		return "", fmt.Errorf("wg genpsk: %w", err)
	}
	if out == "" {
		return "", errEmptyPSK
	}
	return out, nil
}
