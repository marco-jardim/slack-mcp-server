package handler

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/test/util"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestIntegrationConversations(t *testing.T) {
	sseKey := uuid.New().String()
	require.NotEmpty(t, sseKey, "sseKey must be generated for integration tests")
	apiKey := os.Getenv("SLACK_MCP_OPENAI_API")
	require.NotEmpty(t, apiKey, "SLACK_MCP_OPENAI_API must be set for integration tests")

	cfg := util.MCPConfig{
		SSEKey:             sseKey,
		MessageToolEnabled: true,
		MessageToolMark:    true,
	}

	mcp, err := util.SetupMCP(cfg)
	if err != nil {
		t.Fatalf("Failed to set up MCP server: %v", err)
	}
	fwd, err := util.SetupForwarding(context.Background(), "http://"+mcp.Host+":"+strconv.Itoa(mcp.Port))
	if err != nil {
		t.Fatalf("Failed to set up ngrok forwarding: %v", err)
	}
	defer fwd.Shutdown()
	defer mcp.Shutdown()

	client := openai.NewClient(option.WithAPIKey(apiKey))
	ctx := context.Background()

	type matchingRule struct {
		csvFieldName    string
		csvFieldValueRE string
		RowPosition     *int
		TotalRows       *int
	}

	type tc struct {
		name                            string
		input                           string
		expectedToolName                string
		expectedToolOutputMatchingRules []matchingRule
		expectedLLMOutputMatchingRules  []string
	}

	cases := []tc{
		{
			name:             "Test conversations_history tool",
			input:            "Provide a list of slack messages from #testcase-1",
			expectedToolName: "conversations_history",
			expectedToolOutputMatchingRules: []matchingRule{
				{
					csvFieldName:    "Text",
					csvFieldValueRE: "^message 3$",
				},
				{
					csvFieldName:    "Text",
					csvFieldValueRE: "^message 2$",
				},
				{
					csvFieldName:    "Text",
					csvFieldValueRE: "^message 1$",
				},
			},
			expectedLLMOutputMatchingRules: []string{
				"message 1", "message 2", "message 3",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := responses.ResponseNewParams{
				Model: "gpt-4.1-mini",
				Tools: []responses.ToolUnionParam{
					{
						OfMcp: &responses.ToolMcpParam{
							ServerLabel: "slack-mcp-server",
							ServerURL:   fmt.Sprintf("%s://%s/sse", fwd.URL.Scheme, fwd.URL.Host),
							RequireApproval: responses.ToolMcpRequireApprovalUnionParam{
								OfMcpToolApprovalSetting: param.NewOpt("never"),
							},
							Headers: map[string]string{
								"Authorization": "Bearer " + sseKey,
							},
						},
					},
				},
				Input: responses.ResponseNewParamsInputUnion{
					OfString: openai.String(tc.input),
				},
			}

			resp, err := client.Responses.New(ctx, params)
			require.NoError(t, err, "API call failed")

			assert.NotNil(t, resp.Status, "completed")

			var llmOutput strings.Builder
			var toolOutput strings.Builder
			for _, out := range resp.Output {
				if out.Type == "message" && out.Role == "assistant" {
					for _, c := range out.Content {
						if c.Type == "output_text" {
							llmOutput.WriteString(c.Text)
						}
					}
				}
				if out.Type == "mcp_call" && out.Name == tc.expectedToolName {
					toolOutput.WriteString(out.Output)
				}
			}

			require.NotEmpty(t, toolOutput, "no tool output captured")

			// Parse CSV
			reader := csv.NewReader(strings.NewReader(toolOutput.String()))
			rows, err := reader.ReadAll()
			require.NoError(t, err, "failed to parse CSV")

			header := rows[0]
			dataRows := rows[1:]
			colIndex := map[string]int{}
			for i, col := range header {
				colIndex[col] = i
			}

			for _, rule := range tc.expectedToolOutputMatchingRules {
				if rule.TotalRows != nil && *rule.TotalRows > 0 {
					assert.Equalf(t, *rule.TotalRows, len(dataRows),
						"expected %d data rows, got %d", rule.TotalRows, len(dataRows))
				}

				idx, ok := colIndex[rule.csvFieldName]
				require.Truef(t, ok, "CSV did not contain column %q, toolOutput: %q", rule.csvFieldName, toolOutput.String())

				re, err := regexp.Compile(rule.csvFieldValueRE)
				require.NoErrorf(t, err, "invalid regex %q", rule.csvFieldValueRE)

				if rule.RowPosition != nil && *rule.RowPosition >= 0 {
					require.Lessf(t, rule.RowPosition, len(dataRows), "RowPosition %d out of range (only %d data rows)", rule.RowPosition, len(dataRows))
					value := dataRows[*rule.RowPosition][idx]
					assert.Regexpf(t, re, value, "row %d, column %q: expected to match %q, got %q",
						rule.RowPosition, rule.csvFieldName, rule.csvFieldValueRE, value)
					continue
				}

				found := false
				for _, row := range dataRows {
					if idx < len(row) && re.MatchString(row[idx]) {
						found = true
						break
					}
				}
				assert.Truef(t, found, "no row in column %q matched %q; full CSV:\n%s",
					rule.csvFieldName, rule.csvFieldValueRE, toolOutput.String())
			}

			for _, pattern := range tc.expectedLLMOutputMatchingRules {
				re, err := regexp.Compile(pattern)
				require.NoErrorf(t, err, "invalid LLM regex %q", pattern)
				assert.Regexpf(t, re, llmOutput.String(), "LLM output did not match regex %q; output:\n%s",
					pattern, llmOutput.String())
			}
		})
	}
}

func TestUnitParseFlexibleDate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantDate string
		wantErr  bool
	}{
		// Standard formats (existing)
		{
			name:     "YYYY-MM-DD",
			input:    "2025-07-15",
			wantDate: "2025-07-15",
			wantErr:  false,
		},
		{
			name:     "YYYY/MM/DD",
			input:    "2025/07/15",
			wantDate: "2025-07-15",
			wantErr:  false,
		},

		// New flexible month-year formats
		{
			name:     "Month Year - July 2025",
			input:    "July 2025",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "Year Month - 2025 July",
			input:    "2025 July",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "Abbreviated Month Year - Jul 2025",
			input:    "Jul 2025",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "Year Abbreviated Month - 2025 Jul",
			input:    "2025 Jul",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "Case insensitive - july 2025",
			input:    "july 2025",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "Case insensitive - JULY 2025",
			input:    "JULY 2025",
			wantDate: "2025-07-01",
			wantErr:  false,
		},

		// Day-Month-Year formats
		{
			name:     "1-July-2025",
			input:    "1-July-2025",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "July-25-2025",
			input:    "July-25-2025",
			wantDate: "2025-07-25",
			wantErr:  false,
		},
		{
			name:     "July 10 2025",
			input:    "July 10 2025",
			wantDate: "2025-07-10",
			wantErr:  false,
		},
		{
			name:     "10 July 2025",
			input:    "10 July 2025",
			wantDate: "2025-07-10",
			wantErr:  false,
		},
		{
			name:     "31-December-2025",
			input:    "31-December-2025",
			wantDate: "2025-12-31",
			wantErr:  false,
		},
		{
			name:     "2025 July 10",
			input:    "2025 July 10",
			wantDate: "2025-07-10",
			wantErr:  false,
		},

		// Various month names
		{
			name:     "January full name",
			input:    "January 2025",
			wantDate: "2025-01-01",
			wantErr:  false,
		},
		{
			name:     "February abbreviated",
			input:    "Feb 2025",
			wantDate: "2025-02-01",
			wantErr:  false,
		},
		{
			name:     "September with Sept abbreviation",
			input:    "Sept 2025",
			wantDate: "2025-09-01",
			wantErr:  false,
		},

		// Relative dates
		{
			name:     "today",
			input:    "today",
			wantDate: time.Now().UTC().Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "yesterday",
			input:    "yesterday",
			wantDate: time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "Today with capital T",
			input:    "Today",
			wantDate: time.Now().UTC().Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "Yesterday with capital Y",
			input:    "Yesterday",
			wantDate: time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "TODAY all caps",
			input:    "TODAY",
			wantDate: time.Now().UTC().Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "YESTERDAY all caps",
			input:    "YESTERDAY",
			wantDate: time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "tomorrow",
			input:    "tomorrow",
			wantDate: time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "5 days ago",
			input:    "5 days ago",
			wantDate: time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02"),
			wantErr:  false,
		},
		{
			name:     "1 day ago",
			input:    "1 day ago",
			wantDate: time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"),
			wantErr:  false,
		},

		// Edge cases
		{
			name:     "Whitespace trimming",
			input:    "  July 2025  ",
			wantDate: "2025-07-01",
			wantErr:  false,
		},
		{
			name:     "Invalid month name",
			input:    "Jully 2025",
			wantDate: "",
			wantErr:  true,
		},
		{
			name:     "Invalid date format",
			input:    "2025-13-01",
			wantDate: "",
			wantErr:  true,
		},
		{
			name:     "Invalid day for month",
			input:    "31-February-2025",
			wantDate: "",
			wantErr:  true,
		},
		{
			name:     "Empty string",
			input:    "",
			wantDate: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotDate, err := parseFlexibleDate(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseFlexibleDate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && gotDate != tt.wantDate {
				t.Errorf("parseFlexibleDate() gotDate = %v, want %v", gotDate, tt.wantDate)
			}
		})
	}
}

func TestUnitBuildDateFiltersUnit(t *testing.T) {
	tests := []struct {
		name    string
		before  string
		after   string
		on      string
		during  string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "On with flexible format July 2025",
			before:  "",
			after:   "",
			on:      "July 2025",
			during:  "",
			want:    map[string]string{"on": "2025-07-01"},
			wantErr: false,
		},
		{
			name:    "Before and After with flexible formats",
			before:  "December 2025",
			after:   "January 2025",
			on:      "",
			during:  "",
			want:    map[string]string{"before": "2025-12-01", "after": "2025-01-01"},
			wantErr: false,
		},
		{
			name:    "During with day format",
			before:  "",
			after:   "",
			on:      "",
			during:  "15-July-2025",
			want:    map[string]string{"during": "2025-07-15"},
			wantErr: false,
		},
		{
			name:    "Error: on with other filters",
			before:  "2025-12-01",
			after:   "",
			on:      "July 2025",
			during:  "",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Error: during with before",
			before:  "2025-12-01",
			after:   "",
			on:      "",
			during:  "July 2025",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Error: after date is after before date",
			before:  "January 2025",
			after:   "December 2025",
			on:      "",
			during:  "",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Valid: complex date formats",
			before:  "31-December-2025",
			after:   "1-January-2025",
			on:      "",
			during:  "",
			want:    map[string]string{"before": "2025-12-31", "after": "2025-01-01"},
			wantErr: false,
		},
		{
			name:    "Error: invalid date format",
			before:  "",
			after:   "",
			on:      "Jully 2025",
			during:  "",
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildDateFilters(tt.before, tt.after, tt.on, tt.during)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildDateFilters() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("buildDateFilters() got map length = %v, want %v", len(got), len(tt.want))
					return
				}
				for k, v := range tt.want {
					if got[k] != v {
						t.Errorf("buildDateFilters() got[%s] = %v, want %v", k, got[k], v)
					}
				}
			}
		})
	}
}

func TestUnitLimitByExpression_Valid(t *testing.T) {
	now := time.Now()

	oneMonthAgo := now.AddDate(0, -1, 0)
	twoMonthsAgo := now.AddDate(0, -2, 0)

	oneMonthSpan := int64(now.Sub(oneMonthAgo).Seconds())
	twoMonthSpan := int64(now.Sub(twoMonthsAgo).Seconds())

	const tolerance = 86400

	tests := []struct {
		name    string
		input   string
		minSecs int64 // inclusive
		maxSecs int64 // exclusive
	}{
		{"1 day", "", 0, 86400}, // default case with no input test
		{"1 day", "1d", 0, 86400},
		{"2 days", "2d", 86400, 172800},
		{"1 week", "1w", 6 * 86400, 7 * 86400},
		{"2 weeks", "2w", 13 * 86400, 14 * 86400},
		{"1 month", "1m", oneMonthSpan - tolerance, oneMonthSpan + tolerance},
		{"2 months", "2m", twoMonthSpan - tolerance, twoMonthSpan + tolerance},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slackLimit, oldestStr, latestStr, err := limitByExpression(tt.input, defaultConversationsExpressionLimit)
			if err != nil {
				t.Fatalf("expected no error for %q, got %v", tt.input, err)
			}
			if slackLimit != 100 {
				t.Errorf("expected slackLimit=100 for %q, got %d", tt.input, slackLimit)
			}

			// Parse the "1234567890.000000" format back to an integer
			o, err := strconv.ParseInt(strings.TrimSuffix(oldestStr, ".000000"), 10, 64)
			if err != nil {
				t.Fatalf("invalid oldest timestamp %q: %v", oldestStr, err)
			}
			l, err := strconv.ParseInt(strings.TrimSuffix(latestStr, ".000000"), 10, 64)
			if err != nil {
				t.Fatalf("invalid latest timestamp %q: %v", latestStr, err)
			}

			if l <= o {
				t.Errorf("for %q expected latest(%d) > oldest(%d)", tt.input, l, o)
			}
			diff := l - o
			if diff < tt.minSecs || diff >= tt.maxSecs {
				t.Errorf(
					"for %q expected span in [%d, %d), got %d",
					tt.input, tt.minSecs, tt.maxSecs, diff,
				)
			}
		})
	}
}

func TestUnitLimitByExpression_Invalid(t *testing.T) {
	invalid := []string{
		"d",   // too short
		"0d",  // zero
		"-1d", // negative
		"1x",  // bad suffix
		"1",   // missing suffix
		"01",  // no suffix + zero value
	}

	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			_, _, _, err := limitByExpression(input, defaultConversationsExpressionLimit)
			if err == nil {
				t.Errorf("expected error for %q, got nil", input)
			}
		})
	}
}

func TestUnitIsChannelAllowedForConfig(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		config  string
		want    bool
	}{
		// Allow all cases
		{"empty config allows all", "C123", "", true},
		{"true allows all", "C123", "true", true},
		{"1 allows all", "C123", "1", true},

		// Allowlist (whitelist) cases
		{"allowlist - channel in list", "C123", "C123,C456", true},
		{"allowlist - second channel in list", "C456", "C123,C456", true},
		{"allowlist - channel NOT in list", "C789", "C123,C456", false},
		{"allowlist - with spaces", "C123", " C123 , C456 ", true},

		// Blocklist cases
		{"blocklist - channel in list", "C123", "!C123,!C456", false},
		{"blocklist - second channel in list", "C456", "!C123,!C456", false},
		{"blocklist - channel NOT in list", "C789", "!C123,!C456", true},
		{"blocklist - with spaces", "C123", " !C123 , !C456 ", false},

		// Single item cases
		{"single allowlist - match", "C123", "C123", true},
		{"single allowlist - no match", "C456", "C123", false},
		{"single blocklist - match", "C123", "!C123", false},
		{"single blocklist - no match", "C456", "!C123", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isChannelAllowedForConfig(tt.channel, tt.config)
			if got != tt.want {
				t.Errorf("isChannelAllowedForConfig(%q, %q) = %v, want %v",
					tt.channel, tt.config, got, tt.want)
			}
		})
	}
}

func TestUnitIsSlackUserIDPrefix(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"U prefix", "U0123ABCD", true},
		{"W prefix", "W0123ABCD", true},
		{"plain name not ID", "alice", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSlackUserIDPrefix(tt.s)
			if got != tt.want {
				t.Errorf("isSlackUserIDPrefix(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

// fakeMessageWriteSlackAPI is an in-memory provider.SlackAPI for the message write tool
// tests. Only the methods used by conversations_update_message and
// conversations_delete_message are implemented; calling any other method panics.
type fakeMessageWriteSlackAPI struct {
	provider.SlackAPI

	historyMessages []slack.Message
	historyErr      error
	replyMessages   []slack.Message
	repliesErr      error

	historyParams *slack.GetConversationHistoryParameters
	repliesParams *slack.GetConversationRepliesParameters

	historyCalls int
	repliesCalls int
	updateCalls  int
	deleteCalls  int
}

func (f *fakeMessageWriteSlackAPI) GetConversationHistoryContext(_ context.Context, params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	f.historyCalls++
	f.historyParams = params
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return &slack.GetConversationHistoryResponse{Messages: f.historyMessages}, nil
}

func (f *fakeMessageWriteSlackAPI) GetConversationRepliesContext(_ context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	f.repliesCalls++
	f.repliesParams = params
	if f.repliesErr != nil {
		return nil, false, "", f.repliesErr
	}
	return f.replyMessages, false, "", nil
}

func (f *fakeMessageWriteSlackAPI) UpdateMessageContext(_ context.Context, channelID, timestamp string, _ ...slack.MsgOption) (string, string, string, error) {
	f.updateCalls++
	return channelID, timestamp, "", nil
}

func (f *fakeMessageWriteSlackAPI) DeleteMessageContext(_ context.Context, channel, messageTimestamp string) (string, string, error) {
	f.deleteCalls++
	return channel, messageTimestamp, nil
}

func newMessageWriteRequest(args map[string]any) mcp.CallToolRequest {
	var request mcp.CallToolRequest
	request.Params.Arguments = args
	return request
}

func newMessageWriteTestHandler() *ConversationsHandler {
	return &ConversationsHandler{logger: zap.NewNop()}
}

func slackMessage(user, ts string) slack.Message {
	return slack.Message{Msg: slack.Msg{User: user, Timestamp: ts}}
}

func TestUnitMessageWriteToolsChannelPolicy(t *testing.T) {
	const ts = "1700000000.000100"

	parsers := map[string]func(ch *ConversationsHandler, request mcp.CallToolRequest) error{
		updateMessageToolName: func(ch *ConversationsHandler, request mcp.CallToolRequest) error {
			_, err := ch.parseParamsToolUpdateMessage(context.Background(), request)
			return err
		},
		deleteMessageToolName: func(ch *ConversationsHandler, request mcp.CallToolRequest) error {
			_, err := ch.parseParamsToolDeleteMessage(context.Background(), request)
			return err
		},
	}

	for toolName, parse := range parsers {
		t.Run(toolName+" refuses a channel outside the allowlist", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "C111")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C999",
				"ts":         ts,
				"text":       "edited",
			}))
			require.Error(t, err)
			assert.Equal(t,
				fmt.Sprintf("%s tool is not allowed for channel %q, applied policy: %s", toolName, "C999", "C111"),
				err.Error())
		})

		t.Run(toolName+" refuses a blocklisted channel", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "!C999")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C999",
				"ts":         ts,
				"text":       "edited",
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "tool is not allowed for channel \"C999\"")
		})

		t.Run(toolName+" accepts a channel in the allowlist", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "C111,C222")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C222",
				"ts":         ts,
				"text":       "edited",
			}))
			assert.NoError(t, err)
		})

		t.Run(toolName+" is disabled when SLACK_MCP_ADD_MESSAGE_TOOL is empty", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C111",
				"ts":         ts,
				"text":       "edited",
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "by default, the "+toolName+" tool is disabled")
		})

		t.Run(toolName+" is allowed for all channels when listed in SLACK_MCP_ENABLED_TOOLS", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "conversations_history,"+toolName)

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C111",
				"ts":         ts,
				"text":       "edited",
			}))
			assert.NoError(t, err)
		})

		t.Run(toolName+" requires a valid ts", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C111",
				"text":       "edited",
			}))
			require.Error(t, err)
			assert.Equal(t, "ts must be a string", err.Error())

			err = parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"channel_id": "C111",
				"ts":         "1700000000",
				"text":       "edited",
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ts must be a valid timestamp")
		})

		t.Run(toolName+" requires channel_id", func(t *testing.T) {
			t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
			t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

			err := parse(newMessageWriteTestHandler(), newMessageWriteRequest(map[string]any{
				"ts":   ts,
				"text": "edited",
			}))
			require.Error(t, err)
			assert.Equal(t, "channel_id must be a string", err.Error())
		})
	}
}

func TestUnitParseParamsToolUpdateMessage(t *testing.T) {
	const ts = "1700000000.000100"
	t.Setenv("SLACK_MCP_ADD_MESSAGE_TOOL", "true")
	t.Setenv("SLACK_MCP_ENABLED_TOOLS", "")

	t.Run("requires text or blocks", func(t *testing.T) {
		_, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
		}))
		require.Error(t, err)
		assert.Equal(t, "either text or blocks must be provided", err.Error())
	})

	t.Run("empty text and empty blocks are rejected", func(t *testing.T) {
		_, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
			"text":       "",
			"blocks":     "",
		}))
		require.Error(t, err)
		assert.Equal(t, "either text or blocks must be provided", err.Error())
	})

	t.Run("text only is accepted with the default content type", func(t *testing.T) {
		params, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
			"text":       "edited",
		}))
		require.NoError(t, err)
		assert.Equal(t, "C111", params.channel)
		assert.Equal(t, ts, params.ts)
		assert.Equal(t, "edited", params.text)
		assert.Equal(t, "text/markdown", params.contentType)
		assert.Nil(t, params.blocks)
	})

	t.Run("text is sanitised to Slack mrkdwn for both content types", func(t *testing.T) {
		for _, contentType := range []string{"text/markdown", "text/plain"} {
			params, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
				"channel_id":   "C111",
				"ts":           ts,
				"text":         "**done** & [docs](https://example.com)",
				"content_type": contentType,
			}))
			require.NoError(t, err)
			assert.Equal(t, "*done* &amp; <https://example.com|docs>", params.text, contentType)
		}
	})

	t.Run("mrkdwn text objects inside blocks are sanitised", func(t *testing.T) {
		params, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
			"blocks":     `[{"type":"section","text":{"type":"mrkdwn","text":"**bold**"}}]`,
		}))
		require.NoError(t, err)
		require.Len(t, params.blocks, 1)
		section, ok := params.blocks[0].(*slack.SectionBlock)
		require.True(t, ok)
		assert.Equal(t, "*bold*", section.Text.Text)
	})

	t.Run("blocks only are accepted as a JSON string", func(t *testing.T) {
		params, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
			"blocks":     `[{"type":"divider"}]`,
		}))
		require.NoError(t, err)
		assert.Len(t, params.blocks, 1)
		assert.Equal(t, "", params.text)
	})

	t.Run("blocks only are accepted as a raw JSON array", func(t *testing.T) {
		params, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
			"blocks":     []any{map[string]any{"type": "divider"}},
		}))
		require.NoError(t, err)
		assert.Len(t, params.blocks, 1)
	})

	t.Run("invalid blocks JSON is rejected", func(t *testing.T) {
		_, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id": "C111",
			"ts":         ts,
			"blocks":     `not json`,
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocks must be valid Slack Block Kit JSON")
	})

	t.Run("invalid content type is rejected", func(t *testing.T) {
		_, err := newMessageWriteTestHandler().parseParamsToolUpdateMessage(context.Background(), newMessageWriteRequest(map[string]any{
			"channel_id":   "C111",
			"ts":           ts,
			"text":         "edited",
			"content_type": "text/html",
		}))
		require.Error(t, err)
		assert.Equal(t, "content_type must be either 'text/plain' or 'text/markdown'", err.Error())
	})
}

func TestUnitBuildUpdateMessageOptions(t *testing.T) {
	ch := newMessageWriteTestHandler()
	divider := []slack.Block{slack.NewDividerBlock()}

	t.Run("blocks take precedence and text becomes the fallback", func(t *testing.T) {
		options, err := ch.buildUpdateMessageOptions(&updateMessageParams{text: "fallback", contentType: "text/plain", blocks: divider})
		require.NoError(t, err)
		assert.Len(t, options, 2)
	})

	t.Run("blocks without text", func(t *testing.T) {
		options, err := ch.buildUpdateMessageOptions(&updateMessageParams{contentType: "text/markdown", blocks: divider})
		require.NoError(t, err)
		assert.Len(t, options, 1)
	})

	t.Run("plain text clears previous blocks", func(t *testing.T) {
		options, err := ch.buildUpdateMessageOptions(&updateMessageParams{text: "edited", contentType: "text/plain"})
		require.NoError(t, err)
		assert.Len(t, options, 2)
	})

	t.Run("markdown text is sent as mrkdwn text and clears previous blocks", func(t *testing.T) {
		options, err := ch.buildUpdateMessageOptions(&updateMessageParams{text: "*edited*", contentType: "text/markdown"})
		require.NoError(t, err)
		assert.Len(t, options, 2)
	})

	t.Run("unknown content type", func(t *testing.T) {
		_, err := ch.buildUpdateMessageOptions(&updateMessageParams{text: "edited", contentType: "text/html"})
		require.Error(t, err)
	})
}

func TestUnitMessageWriteOwnershipGuard(t *testing.T) {
	const (
		channel = "C123"
		ts      = "1700000000.000100"
		me      = "U_ME"
		other   = "U_OTHER"
	)
	notOwned := fmt.Sprintf("refusing to modify message %s in %s: not authored by the authenticated user", ts, channel)
	updateParams := &updateMessageParams{channel: channel, ts: ts, text: "edited", contentType: "text/plain"}
	deleteParams := &deleteMessageParams{channel: channel, ts: ts}

	t.Run("update refuses a message authored by another user", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{historyMessages: []slack.Message{slackMessage(other, ts)}}

		result, err := newMessageWriteTestHandler().updateOwnMessage(context.Background(), client, me, updateParams)
		require.Error(t, err)
		assert.Nil(t, result)
		assert.Equal(t, notOwned, err.Error())
		assert.Equal(t, 0, client.updateCalls, "chat.update must not be called for a message that is not owned")
	})

	t.Run("delete refuses a message authored by another user", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{historyMessages: []slack.Message{slackMessage(other, ts)}}

		result, err := newMessageWriteTestHandler().deleteOwnMessage(context.Background(), client, me, deleteParams)
		require.Error(t, err)
		assert.Nil(t, result)
		assert.Equal(t, notOwned, err.Error())
		assert.Equal(t, 0, client.deleteCalls, "chat.delete must not be called for a message that is not owned")
	})

	t.Run("bot or system message without a user is refused", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{historyMessages: []slack.Message{slackMessage("", ts)}}

		_, err := newMessageWriteTestHandler().deleteOwnMessage(context.Background(), client, me, deleteParams)
		require.Error(t, err)
		assert.Equal(t, notOwned, err.Error())
		assert.Equal(t, 0, client.deleteCalls)
	})

	t.Run("unknown authenticated user is refused without any API call", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{historyMessages: []slack.Message{slackMessage("", ts)}}

		_, err := newMessageWriteTestHandler().updateOwnMessage(context.Background(), client, "", updateParams)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the authenticated user is unknown")
		assert.Equal(t, 0, client.historyCalls)
		assert.Equal(t, 0, client.updateCalls)
	})

	t.Run("update edits an own top-level message", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{historyMessages: []slack.Message{slackMessage(me, ts)}}

		result, err := newMessageWriteTestHandler().updateOwnMessage(context.Background(), client, me, updateParams)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, 1, client.updateCalls)
		assert.Equal(t, 0, client.repliesCalls, "replies lookup is not needed when history finds the message")

		require.NotNil(t, client.historyParams)
		assert.Equal(t, channel, client.historyParams.ChannelID)
		assert.Equal(t, ts, client.historyParams.Latest)
		assert.Equal(t, ts, client.historyParams.Oldest)
		assert.True(t, client.historyParams.Inclusive)
		assert.Equal(t, 1, client.historyParams.Limit)

		require.Len(t, result.Content, 1)
		textContent, ok := result.Content[0].(mcp.TextContent)
		require.True(t, ok)
		assert.Contains(t, textContent.Text, channel)
		assert.Contains(t, textContent.Text, ts)
	})

	t.Run("delete removes an own thread reply found through conversations.replies", func(t *testing.T) {
		const parentTS = "1699999999.000100"
		client := &fakeMessageWriteSlackAPI{
			historyMessages: nil,
			replyMessages:   []slack.Message{slackMessage(other, parentTS), slackMessage(me, ts)},
		}

		result, err := newMessageWriteTestHandler().deleteOwnMessage(context.Background(), client, me, deleteParams)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, 1, client.historyCalls)
		assert.Equal(t, 1, client.repliesCalls)
		assert.Equal(t, 1, client.deleteCalls)

		require.NotNil(t, client.repliesParams)
		assert.Equal(t, channel, client.repliesParams.ChannelID)
		assert.Equal(t, ts, client.repliesParams.Timestamp)

		require.Len(t, result.Content, 1)
		textContent, ok := result.Content[0].(mcp.TextContent)
		require.True(t, ok)
		assert.Contains(t, textContent.Text, channel)
		assert.Contains(t, textContent.Text, ts)
	})

	t.Run("thread reply authored by another user is refused", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{replyMessages: []slack.Message{slackMessage(me, "1699999999.000100"), slackMessage(other, ts)}}

		_, err := newMessageWriteTestHandler().updateOwnMessage(context.Background(), client, me, updateParams)
		require.Error(t, err)
		assert.Equal(t, notOwned, err.Error())
		assert.Equal(t, 0, client.updateCalls)
	})

	t.Run("message not found", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{repliesErr: errors.New("thread_not_found")}

		_, err := newMessageWriteTestHandler().deleteOwnMessage(context.Background(), client, me, deleteParams)
		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("message %s not found in %s", ts, channel), err.Error())
		assert.Equal(t, 0, client.deleteCalls)
	})

	t.Run("message not found when replies has no exact match", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{replyMessages: []slack.Message{slackMessage(me, "1699999999.000100")}}

		_, err := newMessageWriteTestHandler().updateOwnMessage(context.Background(), client, me, updateParams)
		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("message %s not found in %s", ts, channel), err.Error())
		assert.Equal(t, 0, client.updateCalls)
	})

	t.Run("lookup failure is reported and nothing is modified", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{historyErr: errors.New("channel_not_found")}

		_, err := newMessageWriteTestHandler().updateOwnMessage(context.Background(), client, me, updateParams)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "channel_not_found")
		assert.Equal(t, 0, client.repliesCalls)
		assert.Equal(t, 0, client.updateCalls)
	})

	t.Run("replies lookup failure is reported and nothing is modified", func(t *testing.T) {
		client := &fakeMessageWriteSlackAPI{repliesErr: errors.New("ratelimited")}

		_, err := newMessageWriteTestHandler().deleteOwnMessage(context.Background(), client, me, deleteParams)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ratelimited")
		assert.Equal(t, 0, client.deleteCalls)
	})
}
