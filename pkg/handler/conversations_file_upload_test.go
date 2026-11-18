package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFileUploadSlackAPI is an in-memory provider.SlackAPI for the conversations_add_file
// tests. Only UploadFileContext and GetFileInfoContext are implemented; calling any other
// method panics.
type fakeFileUploadSlackAPI struct {
	provider.SlackAPI

	uploadErr  error
	infoErr    error
	infoResult *slack.File

	uploadCalls int
	infoCalls   int
	gotParams   slack.UploadFileParameters
	gotBody     []byte
}

func (f *fakeFileUploadSlackAPI) UploadFileContext(_ context.Context, params slack.UploadFileParameters) (*slack.FileSummary, error) {
	f.uploadCalls++
	f.gotParams = params
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	body, err := io.ReadAll(params.Reader)
	if err != nil {
		return nil, err
	}
	f.gotBody = body
	return &slack.FileSummary{ID: "F123", Title: params.Title}, nil
}

func (f *fakeFileUploadSlackAPI) GetFileInfoContext(_ context.Context, fileID string, _, _ int) (*slack.File, []slack.Comment, *slack.Paging, error) {
	f.infoCalls++
	if f.infoErr != nil {
		return nil, nil, nil, f.infoErr
	}
	if f.infoResult != nil {
		return f.infoResult, nil, nil, nil
	}
	return &slack.File{ID: fileID}, nil, nil, nil
}

func setAddFileEnv(t *testing.T, policy, maxBytes string) {
	t.Helper()
	t.Setenv("SLACK_MCP_ADD_FILE_TOOL", policy)
	t.Setenv("SLACK_MCP_ADD_FILE_MAX_BYTES", maxBytes)
	t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func TestUnitAddFileMaxBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		want int64
	}{
		"unset":    {"", defaultAddFileMaxBytes},
		"valid":    {"1024", 1024},
		"spaces":   {" 2048 ", 2048},
		"invalid":  {"abc", defaultAddFileMaxBytes},
		"zero":     {"0", defaultAddFileMaxBytes},
		"negative": {"-5", defaultAddFileMaxBytes},
		"float":    {"1.5", defaultAddFileMaxBytes},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_FILE_MAX_BYTES", tc.env)
			assert.Equal(t, tc.want, addFileMaxBytes())
		})
	}
	assert.Equal(t, int64(52428800), defaultAddFileMaxBytes)
}

func TestUnitParseParamsToolAddFileChannelPolicy(t *testing.T) {
	args := func(channel string) map[string]any {
		return map[string]any{"channel_id": channel, "content": "hello", "filename": "a.txt"}
	}

	t.Run("refuses a channel outside the allowlist", func(t *testing.T) {
		setAddFileEnv(t, "C111", "")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C999")))
		require.Error(t, err)
		assert.Equal(t, `conversations_add_file tool is not allowed for channel "C999", applied policy: C111`, err.Error())
	})

	t.Run("refuses a blocklisted channel", func(t *testing.T) {
		setAddFileEnv(t, "!C999", "")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C999")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tool is not allowed for channel \"C999\"")
	})

	t.Run("accepts a blocklist for other channels", func(t *testing.T) {
		setAddFileEnv(t, "!C999", "")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C111")))
		assert.NoError(t, err)
	})

	t.Run("accepts a channel in the allowlist", func(t *testing.T) {
		setAddFileEnv(t, "C111,C222", "")
		p, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C222")))
		require.NoError(t, err)
		assert.Equal(t, "C222", p.channel)
	})

	for _, policy := range []string{"true", "1"} {
		t.Run("policy "+policy+" allows all channels", func(t *testing.T) {
			setAddFileEnv(t, policy, "")
			_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C777")))
			assert.NoError(t, err)
		})
	}

	t.Run("is disabled when the policy is empty", func(t *testing.T) {
		setAddFileEnv(t, "", "")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C111")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "by default, the conversations_add_file tool is disabled")
		assert.Contains(t, err.Error(), "SLACK_MCP_ADD_FILE_TOOL")
	})

	t.Run("empty policy is allow-all when listed in SLACK_MCP_ENABLED_TOOLS", func(t *testing.T) {
		setAddFileEnv(t, "", "")
		t.Setenv("SLACK_MCP_ENABLED_TOOLS", "conversations_history,conversations_add_file")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C111")))
		assert.NoError(t, err)
	})

	t.Run("SLACK_MCP_ADD_MESSAGE_TOOL does not enable the tool", func(t *testing.T) {
		setAddFileEnv(t, "", "")
		t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args("C111")))
		require.Error(t, err)
	})

	t.Run("requires channel_id", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(map[string]any{
			"content": "hello", "filename": "a.txt",
		}))
		require.Error(t, err)
		assert.Equal(t, "channel_id must be a string", err.Error())
	})
}

func TestUnitParseParamsToolAddFileValidation(t *testing.T) {
	parse := func(t *testing.T, args map[string]any) (*addFileParams, error) {
		t.Helper()
		if _, ok := args["channel_id"]; !ok {
			args["channel_id"] = "C111"
		}
		return newMessageWriteTestHandler().parseParamsToolAddFile(context.Background(), newMessageWriteRequest(args))
	}

	t.Run("requires exactly one of file_path and content", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		path := writeTempFile(t, "a.txt", []byte("data"))

		_, err := parse(t, map[string]any{})
		require.Error(t, err)
		assert.Equal(t, "exactly one of file_path or content must be provided", err.Error())

		_, err = parse(t, map[string]any{"file_path": path, "content": "x", "filename": "a.txt"})
		require.Error(t, err)
		assert.Equal(t, "exactly one of file_path or content must be provided", err.Error())
	})

	t.Run("text content", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		p, err := parse(t, map[string]any{
			"content": "hello", "filename": "hello.txt", "title": "T", "initial_comment": "C", "thread_ts": "1700000000.000100",
		})
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), p.content)
		assert.Equal(t, int64(5), p.size)
		assert.Equal(t, "hello.txt", p.filename)
		assert.Equal(t, "T", p.title)
		assert.Equal(t, "C", p.initialComment)
		assert.Equal(t, "1700000000.000100", p.threadTs)
		assert.Empty(t, p.filePath)
	})

	t.Run("explicit text encoding", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		p, err := parse(t, map[string]any{"content": "hello", "filename": "h.txt", "content_encoding": "text"})
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), p.content)
	})

	t.Run("base64 content is decoded", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		raw := []byte{0x00, 0x01, 0xfe, 0xff, 'a'}
		p, err := parse(t, map[string]any{
			"content": base64.StdEncoding.EncodeToString(raw), "filename": "b.bin", "content_encoding": "base64",
		})
		require.NoError(t, err)
		assert.Equal(t, raw, p.content)
		assert.Equal(t, int64(len(raw)), p.size)
	})

	t.Run("invalid base64 is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"content": "***not base64***", "filename": "b.bin", "content_encoding": "base64"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid base64")
	})

	t.Run("unknown content_encoding is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"content": "x", "filename": "a.txt", "content_encoding": "hex"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "content_encoding must be either")
	})

	t.Run("filename is required with content", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"content": "x"})
		require.Error(t, err)
		assert.Equal(t, "filename is required when uploading inline content", err.Error())
	})

	t.Run("filename with path separators is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		for _, name := range []string{"../a.txt", `dir\a.txt`, "dir/a.txt"} {
			_, err := parse(t, map[string]any{"content": "x", "filename": name})
			require.Error(t, err, name)
			assert.Contains(t, err.Error(), "path separators")
		}
	})

	t.Run("empty content counts as missing", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"content": "", "filename": "a.txt"})
		require.Error(t, err)
		assert.Equal(t, "exactly one of file_path or content must be provided", err.Error())
	})

	t.Run("content that decodes to nothing is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"content": "\n", "filename": "a.txt", "content_encoding": "base64"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "zero bytes")
	})

	t.Run("content over the limit is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "10")
		_, err := parse(t, map[string]any{"content": strings.Repeat("a", 11), "filename": "a.txt"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds the limit of 10 bytes")

		p, err := parse(t, map[string]any{"content": strings.Repeat("a", 10), "filename": "a.txt"})
		require.NoError(t, err)
		assert.Equal(t, int64(10), p.size)
	})

	t.Run("base64 limit applies to the decoded length", func(t *testing.T) {
		setAddFileEnv(t, "true", "6")
		// 6 decoded bytes are encoded as 8 characters, which must still fit.
		encoded := base64.StdEncoding.EncodeToString([]byte("abcdef"))
		p, err := parse(t, map[string]any{"content": encoded, "filename": "a.bin", "content_encoding": "base64"})
		require.NoError(t, err)
		assert.Equal(t, int64(6), p.size)

		encoded = base64.StdEncoding.EncodeToString([]byte("abcdefg"))
		_, err = parse(t, map[string]any{"content": encoded, "filename": "a.bin", "content_encoding": "base64"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds the limit of 6 bytes")
	})

	t.Run("invalid thread_ts is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"content": "x", "filename": "a.txt", "thread_ts": "1700000000"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "thread_ts must be a valid timestamp")
	})

	t.Run("file_path uses the base name by default", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		path := writeTempFile(t, "report.csv", []byte("a,b\n1,2\n"))
		p, err := parse(t, map[string]any{"file_path": path})
		require.NoError(t, err)
		assert.Equal(t, path, p.filePath)
		assert.Equal(t, "report.csv", p.filename)
		assert.Equal(t, int64(8), p.size)
		assert.Nil(t, p.content)
	})

	t.Run("file_path honours an explicit filename", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		path := writeTempFile(t, "report.csv", []byte("data"))
		p, err := parse(t, map[string]any{"file_path": path, "filename": "renamed.csv"})
		require.NoError(t, err)
		assert.Equal(t, "renamed.csv", p.filename)
	})

	t.Run("relative file_path is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"file_path": filepath.Join("some", "relative.txt")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be an absolute path")
	})

	t.Run("missing file is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"file_path": filepath.Join(t.TempDir(), "missing.txt")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot access file_path")
	})

	t.Run("directory is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		_, err := parse(t, map[string]any{"file_path": t.TempDir()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is a directory")
	})

	t.Run("empty file is rejected", func(t *testing.T) {
		setAddFileEnv(t, "true", "")
		path := writeTempFile(t, "empty.txt", nil)
		_, err := parse(t, map[string]any{"file_path": path})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})

	t.Run("file over the limit is rejected before reading", func(t *testing.T) {
		setAddFileEnv(t, "true", "4")
		path := writeTempFile(t, "big.txt", []byte("12345"))
		_, err := parse(t, map[string]any{"file_path": path})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds the limit of 4 bytes")

		path = writeTempFile(t, "ok.txt", []byte("1234"))
		p, err := parse(t, map[string]any{"file_path": path})
		require.NoError(t, err)
		assert.Equal(t, int64(4), p.size)
	})

	t.Run("invalid limit falls back to the default", func(t *testing.T) {
		setAddFileEnv(t, "true", "not-a-number")
		path := writeTempFile(t, "ok.txt", []byte("1234"))
		_, err := parse(t, map[string]any{"file_path": path})
		assert.NoError(t, err)
	})
}

func TestUnitUploadFile(t *testing.T) {
	ch := newMessageWriteTestHandler()

	t.Run("uploads inline content into a thread and returns JSON", func(t *testing.T) {
		client := &fakeFileUploadSlackAPI{
			infoResult: &slack.File{ID: "F123", Name: "hello.txt", Title: "Hello", Permalink: "https://example.slack.com/files/U1/F123/hello.txt"},
		}
		params := &addFileParams{
			channel: "C111", threadTs: "1700000000.000100", content: []byte("hello"),
			filename: "hello.txt", title: "Hello", initialComment: "see attached", size: 5,
		}

		res, err := ch.uploadFile(context.Background(), client, params)
		require.NoError(t, err)
		require.NotNil(t, res)

		assert.Equal(t, 1, client.uploadCalls)
		assert.Equal(t, "C111", client.gotParams.Channel)
		assert.Equal(t, "1700000000.000100", client.gotParams.ThreadTimestamp)
		assert.Equal(t, "hello.txt", client.gotParams.Filename)
		assert.Equal(t, "Hello", client.gotParams.Title)
		assert.Equal(t, "see attached", client.gotParams.InitialComment)
		assert.EqualValues(t, 5, client.gotParams.FileSize)
		assert.Equal(t, []byte("hello"), client.gotBody)

		require.Len(t, res.Content, 1)
		text, ok := res.Content[0].(mcp.TextContent)
		require.True(t, ok)
		var got addFileResult
		require.NoError(t, json.Unmarshal([]byte(text.Text), &got))
		assert.Equal(t, addFileResult{
			FileID:    "F123",
			Name:      "hello.txt",
			Title:     "Hello",
			Permalink: "https://example.slack.com/files/U1/F123/hello.txt",
			ChannelID: "C111",
			ThreadTs:  "1700000000.000100",
			SizeBytes: 5,
		}, got)
	})

	t.Run("streams a local file", func(t *testing.T) {
		path := writeTempFile(t, "data.bin", []byte{1, 2, 3, 4})
		client := &fakeFileUploadSlackAPI{}
		res, err := ch.uploadFile(context.Background(), client, &addFileParams{
			channel: "D222", filePath: path, filename: "data.bin", size: 4,
		})
		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Equal(t, []byte{1, 2, 3, 4}, client.gotBody)
		assert.Empty(t, client.gotParams.ThreadTimestamp)
		assert.EqualValues(t, 4, client.gotParams.FileSize)
	})

	t.Run("never sends more than the validated size", func(t *testing.T) {
		path := writeTempFile(t, "grown.bin", []byte("0123456789"))
		client := &fakeFileUploadSlackAPI{}
		_, err := ch.uploadFile(context.Background(), client, &addFileParams{
			channel: "C111", filePath: path, filename: "grown.bin", size: 4,
		})
		require.NoError(t, err)
		assert.Equal(t, []byte("0123"), client.gotBody)
	})

	t.Run("reports a file that vanished before upload", func(t *testing.T) {
		client := &fakeFileUploadSlackAPI{}
		_, err := ch.uploadFile(context.Background(), client, &addFileParams{
			channel: "C111", filePath: filepath.Join(t.TempDir(), "gone.bin"), filename: "gone.bin", size: 4,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to open file_path")
		assert.Zero(t, client.uploadCalls)
	})

	t.Run("missing_scope names the files:write scope", func(t *testing.T) {
		client := &fakeFileUploadSlackAPI{uploadErr: errors.New("missing_scope")}
		_, err := ch.uploadFile(context.Background(), client, &addFileParams{
			channel: "C111", content: []byte("x"), filename: "a.txt", size: 1,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "files:write")
		assert.ErrorIs(t, err, client.uploadErr)
	})

	t.Run("other Slack errors are wrapped and returned", func(t *testing.T) {
		client := &fakeFileUploadSlackAPI{uploadErr: errors.New("channel_not_found")}
		_, err := ch.uploadFile(context.Background(), client, &addFileParams{
			channel: "C111", content: []byte("x"), filename: "a.txt", size: 1,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "channel_not_found")
		assert.NotContains(t, err.Error(), "files:write")
		assert.Zero(t, client.infoCalls)
	})

	t.Run("a permalink lookup failure is reported as a warning, not an error", func(t *testing.T) {
		client := &fakeFileUploadSlackAPI{infoErr: errors.New("file_not_found")}
		res, err := ch.uploadFile(context.Background(), client, &addFileParams{
			channel: "C111", content: []byte("x"), filename: "a.txt", size: 1,
		})
		require.NoError(t, err)
		text, ok := res.Content[0].(mcp.TextContent)
		require.True(t, ok)
		var got addFileResult
		require.NoError(t, json.Unmarshal([]byte(text.Text), &got))
		assert.Equal(t, "F123", got.FileID)
		assert.Empty(t, got.Permalink)
		assert.Contains(t, got.Warning, "file_not_found")
	})
}
