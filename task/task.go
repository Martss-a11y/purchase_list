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
	"encore.dev/pubsub"
	"github.com/gorilla/websocket"
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

// Define pub/sub topic for real-time updates
var TaskUpdates = pubsub.NewTopic("task-updates", pubsub.TopicConfig{
	DeliveryGuarantee: pubsub.AtLeastOnce,
})

// Task update message types
type TaskUpdateMessage struct {
	Type string      `json:"type"` // "created", "updated", "deleted"
	Task *Task       `json:"task,omitempty"`
	ID   *int        `json:"id,omitempty"`
}

// WebSocket connection manager
type ConnectionManager struct {
	connections map[string]chan []byte
	mutex       sync.RWMutex
}

var connManager = &ConnectionManager{
	connections: make(map[string]chan []byte),
}

func (cm *ConnectionManager) AddConnection(id string) chan []byte {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	ch := make(chan []byte, 10)
	cm.connections[id] = ch
	return ch
}

func (cm *ConnectionManager) RemoveConnection(id string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	if ch, exists := cm.connections[id]; exists {
		close(ch)
		delete(cm.connections, id)
	}
}

func (cm *ConnectionManager) Broadcast(message []byte) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()
	for _, ch := range cm.connections {
		select {
		case ch <- message:
		default:
			// Channel is full, skip this connection
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

	// Broadcast the new task to all connected clients
	updateMsg := TaskUpdateMessage{
		Type: "created",
		Task: &newTask,
	}
	if msgBytes, err := json.Marshal(updateMsg); err == nil {
		connManager.Broadcast(msgBytes)
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

	// Update the task object and broadcast
	task.Completed = true
	updateMsg := TaskUpdateMessage{
		Type: "updated",
		Task: &task,
	}
	if msgBytes, err := json.Marshal(updateMsg); err == nil {
		connManager.Broadcast(msgBytes)
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

	// Broadcast the deletion to all connected clients
	taskID, _ := strconv.Atoi(id)
	updateMsg := TaskUpdateMessage{
		Type: "deleted",
		ID:   &taskID,
	}
	if msgBytes, err := json.Marshal(updateMsg); err == nil {
		connManager.Broadcast(msgBytes)
	}

	return nil
}

//encore:api auth method=GET path=/ws
func WebSocketHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// Upgrade the connection to WebSocket
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins for development
		},
	}
	
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		rlog.Error("Failed to upgrade connection", "error", err)
		return
	}
	defer conn.Close()

	// Generate a unique connection ID
	connID := fmt.Sprintf("conn_%d", time.Now().UnixNano())
	updateChan := connManager.AddConnection(connID)
	defer connManager.RemoveConnection(connID)

	// Send initial task list
	tasks, err := GetTasks(ctx)
	if err == nil {
		initialMsg := map[string]interface{}{
			"type": "initial",
			"tasks": tasks.Tasks,
		}
		if msgBytes, err := json.Marshal(initialMsg); err == nil {
			conn.WriteMessage(websocket.TextMessage, msgBytes)
		}
	}

	// Listen for updates and send them to the client
	for {
		select {
		case update := <-updateChan:
			if err := conn.WriteMessage(websocket.TextMessage, update); err != nil {
				rlog.Error("Failed to send update", "error", err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
