package main

import (
	"errors"
	"fmt"
	"reflect"
	"time"
)

// Checkpoint is the latest recoverable Agent state for one Run.
// Messages are kept in model order and exclude the system prompt, which is
// reconstructed from the Agent configuration when the loop resumes.
type Checkpoint struct {
	ID             string                `json:"id"`
	Session        Session               `json:"session"`
	Run            Run                   `json:"run"`
	Messages       []Message             `json:"messages"`
	CompletedTools map[string]ToolResult `json:"completedTools,omitempty"`
	Result         string                `json:"result,omitempty"`
	SavedAt        time.Time             `json:"savedAt"`
}

// NewCheckpoint constructs a checkpoint snapshot from the supplied state.
func NewCheckpoint(session Session, run Run, messages []Message, completedTools map[string]ToolResult) Checkpoint {
	return Checkpoint{
		ID:             run.ID,
		Session:        cloneSession(session),
		Run:            run,
		Messages:       cloneMessages(messages),
		CompletedTools: completedTools,
		Result:         run.Result,
		SavedAt:        time.Now().UTC(),
	}
}

func cloneSession(session Session) Session {
	session.Messages = cloneMessages(session.Messages)
	return session
}

var ErrUnsafeToolResultData = errors.New("tool result data cannot be safely cloned")

func cloneToolResults(results map[string]ToolResult) (map[string]ToolResult, error) {
	if len(results) == 0 {
		return nil, nil
	}
	clone := make(map[string]ToolResult, len(results))
	for key, result := range results {
		data, err := cloneToolData(result.Data)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", key, err)
		}
		result.Data = data
		clone[key] = result
	}
	return clone, nil
}

func cloneToolData(data any) (any, error) {
	if data == nil {
		return nil, nil
	}
	cloned, err := cloneReflectValue(reflect.ValueOf(data), 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeToolResultData, err)
	}
	return cloned.Interface(), nil
}

const maxToolDataCloneDepth = 256

func cloneReflectValue(value reflect.Value, depth int) (reflect.Value, error) {
	if !value.IsValid() {
		return value, nil
	}
	if depth > maxToolDataCloneDepth {
		return reflect.Value{}, fmt.Errorf("nested data exceeds %d levels or contains a cycle", maxToolDataCloneDepth)
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		cloned, err := cloneReflectValue(value.Elem(), depth+1)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		if !cloned.IsValid() || !cloned.Type().AssignableTo(value.Type()) {
			return reflect.Value{}, fmt.Errorf("cloned %s is not assignable to %s", cloned.Type(), value.Type())
		}
		result.Set(cloned)
		return result, nil
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			clonedKey, err := cloneReflectValue(iter.Key(), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			clonedValue, err := cloneReflectValue(iter.Value(), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			if !clonedKey.IsValid() || !clonedKey.Type().AssignableTo(value.Type().Key()) || !clonedValue.IsValid() || !clonedValue.Type().AssignableTo(value.Type().Elem()) {
				return reflect.Value{}, fmt.Errorf("map entry cannot be safely cloned for %s", value.Type())
			}
			result.SetMapIndex(clonedKey, clonedValue)
		}
		return result, nil
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			cloned, err := cloneReflectValue(value.Index(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(cloned)
		}
		return result, nil
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			cloned, err := cloneReflectValue(value.Index(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(i).Set(cloned)
		}
		return result, nil
	case reflect.Ptr:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		result := reflect.New(value.Type().Elem())
		cloned, err := cloneReflectValue(value.Elem(), depth+1)
		if err != nil {
			return reflect.Value{}, err
		}
		result.Elem().Set(cloned)
		return result, nil
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.NumField(); i++ {
			if !value.Field(i).CanInterface() || !result.Field(i).CanSet() {
				return reflect.Value{}, fmt.Errorf("struct field %s.%s is not exported", value.Type(), value.Type().Field(i).Name)
			}
			cloned, err := cloneReflectValue(value.Field(i), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Field(i).Set(cloned)
		}
		return result, nil
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return reflect.Value{}, fmt.Errorf("unsupported value kind %s", value.Kind())
	default:
		return value, nil
	}
}

func cloneCheckpoint(checkpoint Checkpoint) (Checkpoint, error) {
	checkpoint.Session = cloneSession(checkpoint.Session)
	checkpoint.Messages = cloneMessages(checkpoint.Messages)
	completedTools, err := cloneToolResults(checkpoint.CompletedTools)
	if err != nil {
		return Checkpoint{}, err
	}
	checkpoint.CompletedTools = completedTools
	return checkpoint, nil
}
