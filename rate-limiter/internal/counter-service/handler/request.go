package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
)

func decodeJSON(r io.Reader, value any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("request must contain exactly one JSON object")
	}
	return nil
}

func validateQuery(r *http.Request, allowed []string) error {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return err
	}
	for name, values := range values {
		known := false
		for _, field := range allowed {
			if name == field {
				known = true
				break
			}
		}
		if !known || len(values) != 1 {
			return fmt.Errorf("unknown or repeated query parameter %q", name)
		}
	}
	return nil
}

func configIdentity(r *http.Request) (string, int, string, error) {
	if err := validateQuery(r, []string{"service", "port", "api_key"}); err != nil {
		return "", 0, "", err
	}
	q := r.URL.Query()
	service := q.Get("service")
	port, err := strconv.Atoi(q.Get("port"))
	if err != nil || strconv.Itoa(port) != q.Get("port") {
		return "", 0, "", fmt.Errorf("port must be an integer between 1 and 65535")
	}
	if err := models.ValidateService(service, port); err != nil {
		return "", 0, "", err
	}
	return service, port, q.Get("api_key"), nil
}
