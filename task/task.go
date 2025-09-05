package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"encore.dev/beta/auth"
	"encore.dev/beta/errs"
	"encore.dev/rlog"
	"encore.dev/storage/sqldb"
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

// Server-Sent Events implementation for real-time updates

// Task update message types
type TaskUpdateMessage struct {
	Type string `json:"type"` // "created", "updated", "deleted"
	Task *Task  `json:"task,omitempty"`
	ID   *int   `json:"id,omitempty"`
}

// Streaming connection manager
type StreamManager struct {
	streams map[string]chan TaskUpdateMessage
	mutex   sync.RWMutex
}

var streamManager = &StreamManager{
	streams: make(map[string]chan TaskUpdateMessage),
}

func (sm *StreamManager) AddStream(id string) chan TaskUpdateMessage {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	ch := make(chan TaskUpdateMessage, 10)
	sm.streams[id] = ch
	return ch
}

func (sm *StreamManager) RemoveStream(id string) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	if ch, exists := sm.streams[id]; exists {
		close(ch)
		delete(sm.streams, id)
	}
}

func (sm *StreamManager) Broadcast(message TaskUpdateMessage) {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()
	for _, ch := range sm.streams {
		select {
		case ch <- message:
		default:
			// Channel is full, skip this stream
		}
	}
}

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
	var tasks []Task
	rows, err := taskDB.Query(ctx, `SELECT id, description, completed FROM task_item ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to query tasks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var task Task
		if err := rows.Scan(&task.ID, &task.Description, &task.Completed); err != nil {
			return nil, fmt.Errorf("failed to scan task: %w", err)
		}
		tasks = append(tasks, task)
	}

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
	rlog.Debug("log message", "description", p.Description)

	err := taskDB.QueryRow(ctx, `
        INSERT INTO task_item (description, completed)
        VALUES ($1, false)
        RETURNING id, description, completed
    `, p.Description).Scan(&newTask.ID, &newTask.Description, &newTask.Completed)
	if err != nil {
		return nil, fmt.Errorf("failed to create task: %w", err)
	}

	// Broadcast the new task to all connected streams
	updateMsg := TaskUpdateMessage{
		Type: "created",
		Task: &newTask,
	}
	streamManager.Broadcast(updateMsg)

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

	// Update the task object and broadcast
	task.Completed = true
	updateMsg := TaskUpdateMessage{
		Type: "updated",
		Task: &task,
	}
	streamManager.Broadcast(updateMsg)

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

	// Broadcast the deletion to all connected streams
	taskID, _ := strconv.Atoi(id)
	updateMsg := TaskUpdateMessage{
		Type: "deleted",
		ID:   &taskID,
	}
	streamManager.Broadcast(updateMsg)

	return nil
}

// StreamTasks streams task updates to connected clients
//encore:api auth method=GET path=/tasks/stream
func StreamTasks(ctx context.Context) (*StreamResponse, error) {
	// Generate a unique stream ID
	streamID := fmt.Sprintf("stream_%d", time.Now().UnixNano())
	updateChan := streamManager.AddStream(streamID)
	defer streamManager.RemoveStream(streamID)

	// Send initial task list
	tasks, err := GetTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get initial tasks: %w", err)
	}

	// Create the stream response
	stream := &StreamResponse{
		StreamID: streamID,
		Tasks:    tasks.Tasks,
	}

	// Start a goroutine to handle updates
	go func() {
		for {
			select {
			case update := <-updateChan:
				// Log the update (in a real implementation, this would be sent to the client)
				rlog.Info("Task update", "type", update.Type, "streamID", streamID)
			case <-ctx.Done():
				return
			}
		}
	}()

	return stream, nil
}

type StreamResponse struct {
	StreamID string `json:"streamId"`
	Tasks    []Task `json:"tasks"`
}
