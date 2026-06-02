package api

type RealDebridLink struct {
	ID         string `json:"id"`
	Filename   string `json:"filename"`
	MimeType   string `json:"mimeType"`
	Filesize   int64  `json:"filesize"`
	Link       string `json:"link"`
	Host       string `json:"host"`
	HostIcon   string `json:"host_icon"`
	Chunks     int    `json:"chunks"`
	Download   string `json:"download"`
	Streamable int    `json:"streamable"`
}
