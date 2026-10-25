package main

// NewInboundSpec 是新建入站的通用参数。
type NewInboundSpec struct {
	Protocol string `json:"protocol"`
	Network  string `json:"network"`
	Port     int    `json:"port"`
	Remark   string `json:"remark"`
	Path     string `json:"path"`
	Host     string `json:"host"`
	Security string `json:"security"`
	Vision   bool   `json:"vision"`

	ServerName string `json:"server_name"`
	CertFile   string `json:"cert_file"`
	KeyFile    string `json:"key_file"`

	Dest        string `json:"dest"`
	ServerNames string `json:"server_names"`
	ShortID     string `json:"short_id"`
	Fingerprint string `json:"fingerprint"`
}
