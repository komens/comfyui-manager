package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type comfySubmitResponse struct {
	PromptID string `json:"prompt_id"`
}
type comfyImage struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}
type comfyHistory struct {
	Status struct {
		StatusStr string `json:"status_str"`
	} `json:"status"`
	Outputs map[string]struct {
		Images []comfyImage `json:"images"`
	} `json:"outputs"`
}

func submitComfy(ctx context.Context, baseURL string, workflow map[string]any, clientID string) (string, error) {
	body, _ := json.Marshal(map[string]any{"prompt": workflow, "client_id": clientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/prompt", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return "", fmt.Errorf("ComfyUI /prompt HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	var result comfySubmitResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.PromptID == "" {
		return "", fmt.Errorf("ComfyUI response has no prompt_id")
	}
	return result.PromptID, nil
}

func getHistory(ctx context.Context, baseURL, promptID string) (comfyHistory, error) {
	var result map[string]comfyHistory
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/history/"+promptID, nil)
	if err != nil {
		return comfyHistory{}, err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return comfyHistory{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return comfyHistory{}, fmt.Errorf("ComfyUI history HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return comfyHistory{}, err
	}
	return result[promptID], nil
}

func downloadComfyImage(ctx context.Context, baseURL string, image comfyImage, target string) error {
	query := "filename=" + urlQuery(image.Filename) + "&subfolder=" + urlQuery(image.Subfolder) + "&type=" + urlQuery(image.Type)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/view?"+query, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ComfyUI view HTTP %d", response.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".tmp-image-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := io.Copy(temporary, response.Body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, target)
}

func urlQuery(value string) string { return url.QueryEscape(value) }

func (a *app) worker() {
	for taskID := range a.jobs {
		a.executeTask(taskID)
	}
}

func (a *app) executeTask(taskID int64) {
	ctx := context.Background()
	var baseURL, parameters, workflowPath, mappingJSON string
	var itemID int64
	err := a.db.QueryRow(`SELECT t.comfyui_url, t.parameters_json, w.workflow_path, w.mapping_json, i.id FROM generation_tasks t JOIN workflows w ON w.id=t.workflow_id JOIN generation_items i ON i.task_id=t.id WHERE t.id=?`, taskID).Scan(&baseURL, &parameters, &workflowPath, &mappingJSON, &itemID)
	if err != nil {
		a.log.Printf("task %d load failed: %v", taskID, err)
		return
	}
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='running', started_at=? WHERE id=?`, time.Now(), taskID)
	_, _ = a.db.Exec(`UPDATE generation_items SET status='running' WHERE id=?`, itemID)
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "running"})
	workflowBytes, err := os.ReadFile(workflowPath)
	if err == nil {
		var workflow map[string]any
		err = json.Unmarshal(workflowBytes, &workflow)
		if err == nil {
			var input map[string]any
			err = json.Unmarshal([]byte(parameters), &input)
			if err == nil {
				err = injectDirectPrompt(workflow, mappingJSON, input)
			}
			if err == nil {
				var promptID string
				promptID, err = submitComfy(ctx, baseURL, workflow, fmt.Sprintf("comfyui-server-task-%d", taskID))
				if err == nil {
					_, _ = a.db.Exec(`UPDATE generation_items SET comfy_prompt_id=? WHERE id=?`, promptID, itemID)
					err = a.waitAndDownload(ctx, baseURL, promptID, taskID, itemID)
				}
			}
		}
	}
	if err != nil {
		_, _ = a.db.Exec(`UPDATE generation_items SET status='failed', error_message=? WHERE id=?`, err.Error(), itemID)
		_, _ = a.db.Exec(`UPDATE generation_tasks SET status='failed', failed_count=1, completed_at=? WHERE id=?`, time.Now(), taskID)
		a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "failed", "error": err.Error()})
		return
	}
	_, _ = a.db.Exec(`UPDATE generation_items SET status='success' WHERE id=?`, itemID)
	_, _ = a.db.Exec(`UPDATE generation_tasks SET status='completed', success_count=1, completed_at=? WHERE id=?`, time.Now(), taskID)
	a.publish(taskID, map[string]any{"task_id": taskID, "item_id": itemID, "status": "success"})
}

func injectDirectPrompt(workflow map[string]any, mappingJSON string, input map[string]any) error {
	var mapping struct {
		Positive   map[string]string `json:"positive_prompt"`
		Negative   map[string]string `json:"negative_prompt"`
		Parameters map[string]struct {
			NodeID string `json:"node_id"`
			Field  string `json:"field"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(mappingJSON), &mapping); err != nil {
		return err
	}
	positive, _ := input["positive_prompt"].(string)
	negative, _ := input["negative_prompt"].(string)
	if err := setWorkflowField(workflow, mapping.Positive, positive); err != nil {
		return err
	}
	if err := setWorkflowField(workflow, mapping.Negative, negative); err != nil {
		return err
	}
	params, _ := input["parameters"].(map[string]any)
	for name, location := range mapping.Parameters {
		if value, ok := params[name]; ok {
			if err := setWorkflowField(workflow, map[string]string{"node_id": location.NodeID, "field": location.Field}, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func setWorkflowField(workflow map[string]any, location map[string]string, value any) error {
	if location["node_id"] == "" || location["field"] == "" {
		return nil
	}
	node, ok := workflow[location["node_id"]].(map[string]any)
	if !ok {
		return fmt.Errorf("workflow node %s not found", location["node_id"])
	}
	parts := strings.Split(location["field"], ".")
	current := node
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			return fmt.Errorf("workflow field %s not found", location["field"])
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func (a *app) waitAndDownload(ctx context.Context, baseURL, promptID string, taskID, itemID int64) error {
	for attempt := 0; attempt < 720; attempt++ {
		history, err := getHistory(ctx, baseURL, promptID)
		if err != nil {
			return err
		}
		if history.Status.StatusStr == "error" {
			return fmt.Errorf("ComfyUI task failed")
		}
		for _, output := range history.Outputs {
			for index, image := range output.Images {
				target := filepath.Join(a.dataDir, "images", fmt.Sprint(taskID), fmt.Sprint(itemID), fmt.Sprintf("result-%d.png", index+1))
				if err := downloadComfyImage(ctx, baseURL, image, target); err != nil {
					return err
				}
				_, err := a.db.Exec(`INSERT INTO images(generation_item_id, filename, storage_path, created_at) VALUES(?,?,?,?)`, itemID, filepath.Base(target), target, time.Now())
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("ComfyUI task timed out")
}
