package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"encore.dev/beta/auth"
	"encore.dev/beta/errs"
	"encore.dev/rlog"
	"encore.dev/storage/sqldb"
	"encore.dev/pubsub"
)

type Task struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
	Completed   bool   `json:"completed"`
}

//encore:authhandler
func AuthHandler(ctx context.Context, token string) (auth.UID, error) {
	if token == "caroli32" {
		return auth.UID(1), nil
	}
	return "", &errs.Error{
		Code:    errs.Internal,
		Message: "Authentication error",
	}
}

// Define database connection
var taskDB = sqldb.NewDatabase("task_list", sqldb.DatabaseConfig{
	Migrations: "./migrations",
})

// TaskUpdateEvent represents a task update event for real-time updates
type TaskUpdateEvent struct {
	Type string `json:"type"` // "created", "updated", "deleted"
	Task *Task  `json:"task,omitempty"`
	ID   *int   `json:"id,omitempty"`
}

// TaskUpdatesTopic is the Pub/Sub topic for task updates
var TaskUpdatesTopic = pubsub.NewTopic("task-updates", pubsub.TopicConfig{
	DeliveryGuarantee: pubsub.AtLeastOnce,
})

//encore:service
type Service struct {
}

// Shutdown is called when the application is shut down.
func (s *Service) Shutdown(ctx context.Context) error {
	fmt.Println("Shutting down the service")
	// Perform any cleanup here.
	return nil
}

type GetTasksResponse struct {
	Tasks []Task `json:"tasks"`
}

//encore:api auth method=GET path=/tasks
func GetTasks(ctx context.Context) (*GetTasksResponse, error) {
	tasks := make([]Task, 0) // Initialize as empty slice, not nil
	rlog.Info("Starting to fetch tasks from database")
	
	rows, err := taskDB.Query(ctx, `SELECT id, description, completed FROM task_item ORDER BY id`)
	if err != nil {
		rlog.Error("Database query failed", "error", err)
		return nil, fmt.Errorf("failed to query tasks: %w", err)
	}
	defer rows.Close()

	taskCount := 0
	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Description, &task.Completed); err != nil {
			rlog.Error("Failed to scan task", "error", err)
			return nil, fmt.Errorf("failed to scan task: %w", err)
		}
		tasks = append(tasks, task)
		taskCount++
		rlog.Info("Scanned task", "id", task.ID, "description", task.Description, "completed", task.Completed)
	}

	rlog.Info("Finished fetching tasks", "count", taskCount)
	return &GetTasksResponse{Tasks: tasks}, nil
}

type CreateTaskParams struct {
	Description string `json:"description"`
}

type CreateTaskResponse struct {
	Task Task `json:"task"`
}

//encore:api auth method=POST path=/tasks
func CreateTask(ctx context.Context, p *CreateTaskParams) (*CreateTaskResponse, error) {
	var newTask Task
	rlog.Info("Creating new task", "description", p.Description)

	err := taskDB.QueryRow(ctx, `
        INSERT INTO task_item (description, completed)
        VALUES ($1, false)
        RETURNING id, description, completed
    `, p.Description).Scan(&newTask.ID, &newTask.Description, &newTask.Completed)
	if err != nil {
		rlog.Error("Failed to create task", "error", err)
		return nil, fmt.Errorf("failed to create task: %w", err)
	}

	rlog.Info("Successfully created task", "id", newTask.ID, "description", newTask.Description)
	
	// Publish task created event
	_, err = TaskUpdatesTopic.Publish(ctx, &TaskUpdateEvent{
		Type: "created",
		Task: &newTask,
	})
	if err != nil {
		rlog.Error("Failed to publish task created event", "error", err)
	}

	return &CreateTaskResponse{Task: newTask}, nil
}

//encore:api auth method=PATCH path=/tasks/:id
func CompleteTask(ctx context.Context, id string) error {
	rlog.Debug("completing task", "id", id)

	// First get the task to broadcast the update
	var task Task
	err := taskDB.QueryRow(ctx, `
        SELECT id, description, completed FROM task_item WHERE id = $1
    `, id).Scan(&task.ID, &task.Description, &task.Completed)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}

	_, err = taskDB.Exec(ctx, `
        UPDATE task_item
        SET completed = $1
        WHERE id = $2
    `, true, id)
	if err != nil {
		return fmt.Errorf("failed to update task: %w", err)
	}

	// Update the task object and publish event
	task.Completed = true
	_, err = TaskUpdatesTopic.Publish(ctx, &TaskUpdateEvent{
		Type: "updated",
		Task: &task,
	})
	if err != nil {
		rlog.Error("Failed to publish task updated event", "error", err)
	}

	return nil
}

//encore:api auth method=DELETE path=/tasks/:id
func DeleteTask(ctx context.Context, id string) error {
	rlog.Debug("deleting task", "id", id)

	_, err := taskDB.Exec(ctx, `
        DELETE FROM task_item
        WHERE id = $1
    `, id)
	if err != nil {
		return fmt.Errorf("failed to delete task: %w", err)
	}

	// Publish task deleted event
	taskID, _ := strconv.Atoi(id)
	_, err = TaskUpdatesTopic.Publish(ctx, &TaskUpdateEvent{
		Type: "deleted",
		ID:   &taskID,
	})
	if err != nil {
		rlog.Error("Failed to publish task deleted event", "error", err)
	}

	return nil
}

// StreamTasks provides Server-Sent Events for real-time task updates
//encore:api auth raw method=GET path=/tasks/stream
func StreamTasks(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	// Set headers for Server-Sent Events
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")

	// Send initial task list
	tasks, err := GetTasks(ctx)
	if err != nil {
		rlog.Error("Failed to get initial tasks for stream", "error", err)
		http.Error(w, "Failed to get initial tasks", http.StatusInternalServerError)
		return
	}

	// Send initial data
	initialData := map[string]interface{}{
		"type":  "initial",
		"tasks": tasks.Tasks,
	}
	if data, err := json.Marshal(initialData); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}

	// Create a subscription to the task updates topic
	subscription := pubsub.NewSubscription(
		TaskUpdatesTopic, "task-stream-subscription",
		pubsub.SubscriptionConfig[*TaskUpdateEvent]{
			Handler: func(ctx context.Context, event *TaskUpdateEvent) error {
				// Write the event to the response
				if data, err := json.Marshal(event); err == nil {
					fmt.Fprintf(w, "data: %s\n\n", data)
					w.(http.Flusher).Flush()
				}
				return nil
			},
		},
	)

	// Start the subscription
	if err := subscription.Start(ctx); err != nil {
		rlog.Error("Failed to start subscription", "error", err)
		return
	}

	// Wait for the context to be done (client disconnect)
	<-ctx.Done()
	rlog.Info("Client disconnected from stream")
}
