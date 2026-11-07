package mcptools

import (
	"encoding/json"
	"fmt"
)

// NewTool builds a host-owned tool declaration with its input schema.
func NewTool(name, description, schema string) Tool {
	return Tool{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

type toolArgumentsError struct{ message string }

func (e *toolArgumentsError) Error() string { return e.message }
func (e *toolArgumentsError) SafeToolFailure() (string, string) {
	return "tool_arguments_invalid", e.message + "；校验阶段未执行工具，可按提示更正参数"
}

// ArgumentError describes a failure before any tool was executed.
func ArgumentError(path, message string) error {
	return &toolArgumentsError{fmt.Sprintf("参数 %s：%s", path, message)}
}
