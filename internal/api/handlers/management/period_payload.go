package management

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota"
)

func validatePeriodPayloadJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return err
	}
	return validatePeriodPayloadValue(root)
}

func validatePeriodPayloadValue(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if err := validatePeriodPayloadValue(item); err != nil {
				return err
			}
		}
	case map[string]any:
		if items, ok := typed["items"]; ok {
			if err := validatePeriodPayloadValue(items); err != nil {
				return err
			}
		}
		var legacy *float64
		if raw, ok := typed["daily-spending-limit"]; ok {
			value, err := jsonNumberFloat(raw)
			if err != nil {
				return fmt.Errorf("daily-spending-limit: %w", err)
			}
			legacy = &value
		}
		var patch *quota.PeriodSpendingLimitsPatch
		if raw, ok := typed["period-spending-limits"]; ok {
			object, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("period-spending-limits must be an object")
			}
			patch = &quota.PeriodSpendingLimitsPatch{}
			for key, target := range map[string]**float64{
				"5h": &patch.FiveHour, "day": &patch.Day, "week": &patch.Week, "month": &patch.Month,
			} {
				rawValue, exists := object[key]
				if !exists {
					continue
				}
				parsed, err := jsonNumberFloat(rawValue)
				if err != nil {
					return fmt.Errorf("period-spending-limits.%s: %w", key, err)
				}
				*target = &parsed
			}
		}
		if _, err := quota.ResolveLegacyDay(legacy, patch); err != nil {
			return err
		}
	}
	return nil
}

func jsonNumberFloat(value any) (float64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("must be a number")
	}
	parsed, err := number.Float64()
	if err != nil {
		return 0, err
	}
	return parsed, nil
}
