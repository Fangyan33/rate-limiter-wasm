package auth

import (
	"errors"
	"strings"
)

var (
	ErrMissingAuthorizationHeader = errors.New("authorization header is missing")
	ErrInvalidAuthorizationFormat = errors.New("authorization header must use Bearer <api_key>")
	ErrEmptyBearerToken           = errors.New("bearer token is empty")
)

func ParseBearerToken(headerValue string) (string, error) {
	if headerValue == "" || strings.TrimSpace(headerValue) == "" {
		return "", ErrMissingAuthorizationHeader
	}

	if headerValue == "Bearer" || headerValue == "Bearer " {
		return "", ErrEmptyBearerToken
	}

	if !strings.HasPrefix(headerValue, "Bearer ") {
		return "", ErrInvalidAuthorizationFormat
	}

	token := strings.TrimPrefix(headerValue, "Bearer ")
	if token == "" {
		return "", ErrEmptyBearerToken
	}

	if strings.ContainsAny(token, " \t\r\n") {
		return "", ErrInvalidAuthorizationFormat
	}

	return token, nil
}
