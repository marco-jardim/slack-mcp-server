package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const (
	addFileToolName    = "conversations_add_file"
	addFilePolicyEnv   = "SLACK_MCP_ADD_FILE_TOOL"
	addFileMaxBytesEnv = "SLACK_MCP_ADD_FILE_MAX_BYTES"

	// defaultAddFileMaxBytes is the upload size limit (50 MiB) used when
	// SLACK_MCP_ADD_FILE_MAX_BYTES is unset, invalid or not positive.
	defaultAddFileMaxBytes int64 = 50 * 1024 * 1024

	contentEncodingText   = "text"
	contentEncodingBase64 = "base64"
)

type addFileParams struct {
	channel        string
	threadTs       string
	filePath       string // absolute local path; empty when content is used
	content        []byte // inline content; nil when filePath is used
	filename       string
	title          string
	initialComment string
	size           int64
}

// addFileResult is the JSON payload returned by conversations_add_file.
type addFileResult struct {
	FileID    string `json:"file_id"`
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	Permalink string `json:"permalink,omitempty"`
	ChannelID string `json:"channel_id"`
	ThreadTs  string `json:"thread_ts,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	Warning   string `json:"warning,omitempty"`
}

// addFileMaxBytes returns the configured upload size limit.
func addFileMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(addFileMaxBytesEnv))
	if raw == "" {
		return defaultAddFileMaxBytes
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return defaultAddFileMaxBytes
	}
	return v
}

// ConversationsAddFileHandler uploads a file (from a local path or inline content) to a
// channel or DM, optionally into a thread, and returns the file details as JSON.
func (ch *ConversationsHandler) ConversationsAddFileHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ch.logger.Debug("ConversationsAddFileHandler called", zap.Any("params", sanitizedAddFileLogParams(request)))

	// provider readiness
	if ready, err := ch.apiProvider.IsReady(); !ready {
		ch.logger.Error("API provider not ready", zap.Error(err))
		return nil, err
	}

	params, err := ch.parseParamsToolAddFile(ctx, request)
	if err != nil {
		ch.logger.Error("Failed to parse add-file params", zap.Error(err))
		return nil, err
	}

	return ch.uploadFile(ctx, ch.apiProvider.Slack(), params)
}

// sanitizedAddFileLogParams returns the request arguments without the inline content, which
// may be very large or sensitive.
func sanitizedAddFileLogParams(request mcp.CallToolRequest) map[string]any {
	out := make(map[string]any)
	for k, v := range request.GetArguments() {
		if k == "content" {
			continue
		}
		out[k] = v
	}
	return out
}

// uploadFile sends the file described by params to Slack and builds the tool result.
func (ch *ConversationsHandler) uploadFile(ctx context.Context, client provider.SlackAPI, params *addFileParams) (*mcp.CallToolResult, error) {
	var reader io.Reader
	if params.filePath != "" {
		f, err := os.Open(params.filePath)
		if err != nil {
			ch.logger.Error("Failed to open file for upload", zap.String("file_path", params.filePath), zap.Error(err))
			return nil, fmt.Errorf("failed to open file_path %q: %w", params.filePath, err)
		}
		defer f.Close()
		// Never send more than the size that was validated against the limit.
		reader = io.LimitReader(f, params.size)
	} else {
		reader = bytes.NewReader(params.content)
	}

	uploadParams := slack.UploadFileParameters{
		Reader:          reader,
		FileSize:        int(params.size),
		Filename:        params.filename,
		Title:           params.title,
		InitialComment:  params.initialComment,
		Channel:         params.channel,
		ThreadTimestamp: params.threadTs,
	}

	ch.logger.Debug("Uploading Slack file",
		zap.String("channel", params.channel),
		zap.String("thread_ts", params.threadTs),
		zap.String("filename", params.filename),
		zap.Int64("size", params.size),
	)
	summary, err := client.UploadFileContext(ctx, uploadParams)
	if err != nil {
		ch.logger.Error("Slack UploadFileContext failed", zap.Error(err))
		return nil, wrapFileUploadError(err)
	}
	if summary == nil || summary.ID == "" {
		return nil, errors.New("slack file upload returned no file id")
	}

	result := addFileResult{
		FileID:    summary.ID,
		Name:      params.filename,
		Title:     summary.Title,
		ChannelID: params.channel,
		ThreadTs:  params.threadTs,
		SizeBytes: params.size,
	}

	// The upload response carries no permalink; fetch it for convenience. The upload has
	// already succeeded at this point, so a failure here is logged and reported as a warning
	// in the result instead of failing the call.
	info, _, _, infoErr := client.GetFileInfoContext(ctx, summary.ID, 0, 0)
	switch {
	case infoErr != nil:
		ch.logger.Warn("Uploaded file, but could not read its permalink",
			zap.String("file_id", summary.ID), zap.Error(infoErr))
		result.Warning = "file uploaded, but its permalink could not be fetched: " + infoErr.Error()
	case info != nil:
		result.Permalink = info.Permalink
		if info.Name != "" {
			result.Name = info.Name
		}
		if info.Title != "" {
			result.Title = info.Title
		}
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to encode upload result: %w", err)
	}
	return mcp.NewToolResultText(string(out)), nil
}

// wrapFileUploadError turns Slack API failures into actionable errors.
func wrapFileUploadError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "missing_scope"):
		return fmt.Errorf("slack file upload failed: %w (the token is missing the files:write scope; add it to the Slack app or user token)", err)
	case strings.Contains(msg, "not_in_channel"):
		return fmt.Errorf("slack file upload failed: %w (the authenticated user is not a member of the target channel)", err)
	default:
		return fmt.Errorf("slack file upload failed: %w", err)
	}
}

// parseParamsToolAddFile validates the tool arguments and applies the
// SLACK_MCP_ADD_FILE_TOOL channel policy (same syntax as SLACK_MCP_ADD_MESSAGE_TOOL).
func (ch *ConversationsHandler) parseParamsToolAddFile(ctx context.Context, request mcp.CallToolRequest) (*addFileParams, error) {
	disabledMsg := "by default, the " + addFileToolName + " tool is disabled to guard Slack workspaces against accidental file uploads. " +
		"To enable it, set the " + addFilePolicyEnv + " environment variable to true, 1, or comma separated list of channels " +
		"to limit where the MCP can upload files, e.g. 'SLACK_MCP_ADD_FILE_TOOL=C1234567890,D0987654321', 'SLACK_MCP_ADD_FILE_TOOL=!C1234567890' " +
		"to enable all except one or 'SLACK_MCP_ADD_FILE_TOOL=true' for all channels and DMs"

	channel, err := ch.resolveAuthorizedWriteChannel(ctx, request, addFileToolName, addFilePolicyEnv, disabledMsg)
	if err != nil {
		return nil, err
	}

	threadTs := request.GetString("thread_ts", "")
	if threadTs != "" && !strings.Contains(threadTs, ".") {
		ch.logger.Error("Invalid thread_ts format", zap.String("thread_ts", threadTs))
		return nil, errors.New("thread_ts must be a valid timestamp in format 1234567890.123456")
	}

	filePath := request.GetString("file_path", "")
	content := request.GetString("content", "")
	if (filePath == "") == (content == "") {
		return nil, errors.New("exactly one of file_path or content must be provided")
	}

	filename := request.GetString("filename", "")
	if strings.ContainsAny(filename, `/\`) {
		return nil, errors.New("filename must be a plain file name without path separators")
	}

	maxBytes := addFileMaxBytes()
	params := &addFileParams{
		channel:        channel,
		threadTs:       threadTs,
		filename:       filename,
		title:          request.GetString("title", ""),
		initialComment: request.GetString("initial_comment", ""),
	}

	if filePath != "" {
		if !filepath.IsAbs(filePath) {
			return nil, fmt.Errorf("file_path must be an absolute path, got %q", filePath)
		}
		info, err := os.Stat(filePath)
		if err != nil {
			return nil, fmt.Errorf("cannot access file_path %q: %w", filePath, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("file_path %q is a directory, not a file", filePath)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("file_path %q is not a regular file", filePath)
		}
		if info.Size() == 0 {
			return nil, fmt.Errorf("file_path %q is empty, Slack does not accept empty files", filePath)
		}
		if info.Size() > maxBytes {
			return nil, fmt.Errorf("file_path %q is %d bytes, which exceeds the limit of %d bytes (%s)", filePath, info.Size(), maxBytes, addFileMaxBytesEnv)
		}
		params.filePath = filePath
		params.size = info.Size()
		if params.filename == "" {
			params.filename = filepath.Base(filePath)
		}
		return params, nil
	}

	// Inline content.
	if params.filename == "" {
		return nil, errors.New("filename is required when uploading inline content")
	}
	encoding := request.GetString("content_encoding", contentEncodingText)
	var data []byte
	switch encoding {
	case contentEncodingText:
		data = []byte(content)
	case contentEncodingBase64:
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("content is not valid base64: %w", err)
		}
		data = decoded
	default:
		return nil, fmt.Errorf("content_encoding must be either %q or %q, got %q", contentEncodingText, contentEncodingBase64, encoding)
	}
	if len(data) == 0 {
		return nil, errors.New("content decodes to zero bytes, Slack does not accept empty files")
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("content is %d bytes, which exceeds the limit of %d bytes (%s)", len(data), maxBytes, addFileMaxBytesEnv)
	}
	params.content = data
	params.size = int64(len(data))
	return params, nil
}
