package tools

import (
	"context"
	"time"
)

// GetTodayTool returns the current date and time.
type GetTodayTool struct{}

// Name returns the tool name.
func (t *GetTodayTool) Name() string {
	return "get_today"
}

// Description returns the tool description.
func (t *GetTodayTool) Description() string {
	return "Get the current date and time. Useful for time-sensitive tasks."
}

// Parameters returns the JSON schema for the tool parameters.
func (t *GetTodayTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"format": map[string]interface{}{
				"type":        "string",
				"description": "Date format: 'full', 'date', 'time', or 'iso'",
				"enum":        []string{"full", "date", "time", "iso"},
			},
		},
	}
}

// Execute returns the current date/time.
func (t *GetTodayTool) Execute(ctx context.Context, params map[string]interface{}) (string, error) {
	format := "full"
	if f, ok := params["format"].(string); ok && f != "" {
		format = f
	}

	now := time.Now()

	switch format {
	case "date":
		return now.Format("2006-01-02"), nil
	case "time":
		return now.Format("15:04:05"), nil
	case "iso":
		return now.Format(time.RFC3339), nil
	case "full":
		return now.Format("Monday, January 2, 2006 at 3:04 PM MST"), nil
	default:
		return now.Format("2006-01-02 15:04:05 MST"), nil
	}
}
