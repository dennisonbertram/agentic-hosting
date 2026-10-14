package builder

type brokerRequest struct {
	Operation string `json:"operation"`
	BuildID   string `json:"build_id"`
}

type brokerResponse struct {
	OK             bool   `json:"ok"`
	Error          string `json:"error,omitempty"`
	ActiveState    string `json:"active_state,omitempty"`
	Result         string `json:"result,omitempty"`
	ExecMainStatus int    `json:"exec_main_status,omitempty"`
	MainPID        int    `json:"main_pid,omitempty"`
}
