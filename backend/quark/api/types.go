// Package api contains definitions for using the Quark drive API.
//
// The wire protocol is a faithful port of the OpenList quark_uc driver
// so that uploads keep working even if upstream rclone drifts. Do NOT
// "clean up" any of the quirks (e.g. the upload_url slice) without
// re-validating against a real account.
package api

// Resp is the envelope returned by every endpoint
type Resp struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// File is a file or folder metadata entry
type File struct {
	Fid        string `json:"fid"`
	FileName   string `json:"file_name"`
	Category   int    `json:"category"`
	Size       int64  `json:"size"`
	LCreatedAt int64  `json:"l_created_at"`
	LUpdatedAt int64  `json:"l_updated_at"`
	File       bool   `json:"file"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// SortResp is the response to /file/sort
type SortResp struct {
	Resp
	Data struct {
		List []File `json:"list"`
	} `json:"data"`
	Metadata struct {
		Size  int    `json:"_size"`
		Page  int    `json:"_page"`
		Count int    `json:"_count"`
		Total int    `json:"_total"`
		Way   string `json:"way"`
	} `json:"metadata"`
}

// DownResp is the response to /file/download
type DownResp struct {
	Resp
	Data []struct {
		DownloadUrl string `json:"download_url"`
	} `json:"data"`
}

// UpPreResp is the response to /file/upload/pre
type UpPreResp struct {
	Resp
	Data struct {
		TaskId    string `json:"task_id"`
		Finish    bool   `json:"finish"`
		UploadId  string `json:"upload_id"`
		ObjKey    string `json:"obj_key"`
		UploadUrl string `json:"upload_url"`
		Fid       string `json:"fid"`
		Bucket    string `json:"bucket"`
		Callback  struct {
			CallbackUrl  string `json:"callbackUrl"`
			CallbackBody string `json:"callbackBody"`
		} `json:"callback"`
		FormatType string `json:"format_type"`
		Size       int    `json:"size"`
		AuthInfo   string `json:"auth_info"`
	} `json:"data"`
	Metadata struct {
		PartThread int    `json:"part_thread"`
		Acc2       string `json:"acc2"`
		Acc1       string `json:"acc1"`
		PartSize   int    `json:"part_size"`
	} `json:"metadata"`
}

// UpAuthResp is the response to /file/upload/auth
type UpAuthResp struct {
	Resp
	Data struct {
		AuthKey string        `json:"auth_key"`
		Speed   int           `json:"speed"`
		Headers []interface{} `json:"headers"`
	} `json:"data"`
}

// HashResp is the response to /file/update/hash
type HashResp struct {
	Resp
	Data struct {
		Finish     bool   `json:"finish"`
		Fid        string `json:"fid"`
		Thumbnail  string `json:"thumbnail"`
		FormatType string `json:"format_type"`
	} `json:"data"`
}

// MemberResp is the response to /member
type MemberResp struct {
	Resp
	Data struct {
		UseCapacity         int64 `json:"use_capacity"`
		TotalCapacity       int64 `json:"total_capacity"`
		SecretUseCapacity   int64 `json:"secret_use_capacity"`
		SecretTotalCapacity int64 `json:"secret_total_capacity"`
	} `json:"data"`
}

// CreateResp is the response to the file/folder creation endpoint
type CreateResp struct {
	Resp
	Data struct {
		Fid string `json:"fid"`
	} `json:"data"`
}
