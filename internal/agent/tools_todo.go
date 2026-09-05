package agent

// The todo tool (§9.1): the model maintains the task list with full-list
// replacement semantics; the engine surfaces changes to the sink.

import (
	"context"
	"fmt"
)

// todoStatuses are the legal todo status values.
var todoStatuses = map[string]bool{"pending": true, "in_progress": true, "completed": true}

func todoTool() Tool {
	return Tool{
		Name: "todo",
		Description: "Maintain the task list: pass the FULL list every time (it replaces the previous one). " +
			"For multi-step tasks (3+ steps) plan with todo first; mark an item completed as soon as it is done, " +
			"and confirm the previous item is completed before starting the next.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "The complete task list; status is pending | in_progress | completed",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":      map[string]any{"type": "string"},
							"content": map[string]any{"type": "string"},
							"status":  map[string]any{"type": "string"},
						},
						"required": []string{"id", "content", "status"},
					},
				},
			},
			"required": []string{"todos"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			raw, ok := args["todos"].([]any)
			if !ok {
				return Result{}, fmt.Errorf("参数 todos 必须是数组")
			}
			todos := make([]Todo, 0, len(raw))
			for i, item := range raw {
				m, ok := item.(map[string]any)
				if !ok {
					return Result{}, fmt.Errorf("todos[%d] 不是对象", i)
				}
				id, _ := m["id"].(string)
				content, _ := m["content"].(string)
				status, _ := m["status"].(string)
				if id == "" || content == "" {
					return Result{}, fmt.Errorf("todos[%d] 缺少 id 或 content", i)
				}
				if !todoStatuses[status] {
					return Result{}, fmt.Errorf("todos[%d] 的 status 必须是 pending|in_progress|completed，得到 %q", i, status)
				}
				todos = append(todos, Todo{ID: id, Content: content, Status: status})
			}
			task.SetTodos(todos)
			return Result{Meta: fmt.Sprintf("%d 项", len(todos)), Display: "todo"}, nil
		},
	}
}
