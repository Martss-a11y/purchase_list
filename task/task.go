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
	rlog.Info("Added stream", "id", id, "totalStreams", len(sm.streams))
	return ch
}

func (sm *StreamManager) RemoveStream(id string) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	if ch, exists := sm.streams[id]; exists {
		close(ch)
		delete(sm.streams, id)
		rlog.Info("Removed stream", "id", id, "totalStreams", len(sm.streams))
	}
}

func (sm *StreamManager) Broadcast(message TaskUpdateMessage) {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()
	rlog.Info("Broadcasting message", "type", message.Type, "streams", len(sm.streams))
	for _, ch := range sm.streams {
		select {
		case ch <- message:
			rlog.Info("Message sent to stream")
		default:
			rlog.Warn("Channel is full, skipping stream")
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

// StreamTasks provides Server-Sent Events for real-time task updates
//encore:api raw method=GET path=/tasks/stream
func StreamTasks(w http.ResponseWriter, r *http.Request) {
	// Set headers for Server-Sent Events
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")

	// Generate a unique stream ID
	streamID := fmt.Sprintf("stream_%d", time.Now().UnixNano())
	updateChan := streamManager.AddStream(streamID)
	defer streamManager.RemoveStream(streamID)

	// Send initial task list
	tasks, err := GetTasks(r.Context())
	if err != nil {
		rlog.Error("Failed to get initial tasks", "error", err)
		return
	}

	// Send initial data
	initialData := map[string]interface{}{
		"streamId": streamID,
		"tasks":    tasks.Tasks,
	}
	if data, err := json.Marshal(initialData); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
	}

	// Listen for updates and send them to the client
	for {
		select {
		case update := <-updateChan:
			if data, err := json.Marshal(update); err == nil {
				fmt.Fprintf(w, "data: %s\n\n", data)
				w.(http.Flusher).Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}
