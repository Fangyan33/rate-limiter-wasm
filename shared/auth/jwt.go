package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	maxAuthorizationHeaderBytes = 16 * 1024
	maxUIDBytes                 = 64
)

func ParseUIDFromJWT(authorizationHeader string) (string, error) {
	if len(authorizationHeader) > maxAuthorizationHeaderBytes {
		return "", fmt.Errorf("authorization header too large")
	}

	jwt, err := ParseBearerToken(authorizationHeader)
	if err != nil {
		return "", err
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid jwt format")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode jwt payload: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", fmt.Errorf("parse jwt payload: %w", err)
	}

	rawUID, ok := claims["uid"]
	if !ok {
		return "", fmt.Errorf("missing uid")
	}

	var uid string
	switch v := rawUID.(type) {
	case string:
		uid = v
	case float64:
		if v != math.Trunc(v) {
			return "", fmt.Errorf("uid must be an integer")
		}
		uid = strconv.FormatInt(int64(v), 10)
	default:
		return "", fmt.Errorf("unsupported uid type")
	}

	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "", fmt.Errorf("uid must not be empty")
	}
	if len(uid) > maxUIDBytes {
		return "", fmt.Errorf("uid too long")
	}
	if strings.ContainsAny(uid, "\n\r\t") {
		return "", fmt.Errorf("uid contains invalid whitespace")
	}

	return uid, nil
}
