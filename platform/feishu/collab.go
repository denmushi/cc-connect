package feishu

// Group collaboration mode ("collab"): several people's coding-agent bots
// discuss in one Feishu group. When a bot is @-mentioned (by a person or by
// another bot) it first pulls every group message since its previous turn,
// replies with a Markdown card, and may hand the discussion over by
// @-mentioning people and their bots inside that card. Feishu delivers <at>
// tags inside bot-sent cards to the mentioned bot, so no extra plain-text
// message is needed. A loop guard stops bot-to-bot hand-offs once too many
// bot messages follow each other without a human speaking.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/chenhg5/cc-connect/core"
)

const (
	collabDefaultInitialHistory = 200
	collabDefaultMaxBotTurns    = 6
	collabMaxIncrementalFetch   = 500   // safety cap for one incremental pull
	collabMaxMessageChars       = 4000  // per message in the transcript
	collabMaxTranscriptChars    = 80000 // whole transcript; oldest lines are dropped first
	collabPageSize              = 50
)

// collabRuntime holds collab settings and per-chat state for one platform.
type collabRuntime struct {
	initialHistory int
	maxBotTurns    int
	stateDir       string // empty = keep state in memory only

	mu        sync.Mutex
	chats     map[string]*collabChatState
	chatLocks map[string]*sync.Mutex
	selfName  string
	ownerID   string   // owner's open_id (collab_owner option, else the app owner)
	members   sync.Map // chatID -> *collabMembersEntry
}

// collabRosterEntry describes one bot that introduced itself with /role.
type collabRosterEntry struct {
	Name   string `json:"name"`
	AppID  string `json:"app_id"`
	OpenID string `json:"open_id"` // the bot's own open_id; other apps can @ it with this id
	Owner  string `json:"owner"`
	Role   string `json:"role"`
}

// collabChatState is persisted per chat so a restart does not re-read history.
type collabChatState struct {
	// ConversationID is the agent conversation the watermark belongs to (core
	// session ID). A new conversation (/new, /switch, /fork, idle rotation)
	// starts reading from scratch.
	ConversationID string                        `json:"conversation_id"`
	WatermarkMs    int64                         `json:"watermark_ms"`
	Role           string                        `json:"role"`
	Owner          string                        `json:"owner"`
	Roster         map[string]*collabRosterEntry `json:"roster"`        // app_id -> entry
	MentionNames   map[string]string             `json:"mention_names"` // open_id (this app's view) -> display name
}

type collabMention struct {
	Key  string `json:"key"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// collabMsg is one message returned by the Feishu message history API.
type collabMsg struct {
	ID         string
	MsgType    string
	CreateMs   int64
	SenderType string // "user", "app" or "system"
	SenderID   string // open_id for users, app_id for bots
	Content    string
	Mentions   []collabMention
	Deleted    bool
}

func newCollabRuntime(opts map[string]any) *collabRuntime {
	if enabled, _ := opts["collab"].(bool); !enabled {
		return nil
	}
	c := &collabRuntime{
		initialHistory: collabIntOpt(opts, "collab_initial_history", collabDefaultInitialHistory),
		maxBotTurns:    collabIntOpt(opts, "collab_max_bot_turns", collabDefaultMaxBotTurns),
		chats:          make(map[string]*collabChatState),
		chatLocks:      make(map[string]*sync.Mutex),
	}
	c.ownerID, _ = opts["collab_owner"].(string)
	if dataDir, _ := opts["cc_data_dir"].(string); dataDir != "" {
		project, _ := opts["cc_project"].(string)
		if project == "" {
			project = "default"
		}
		c.stateDir = filepath.Join(dataDir, "feishu-collab", project)
	}
	return c
}

func collabIntOpt(opts map[string]any, key string, def int) int {
	switch v := opts[key].(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return def
}

// lockChat serializes collab work per chat; callers must defer the returned unlock.
func (c *collabRuntime) lockChat(chatID string) func() {
	c.mu.Lock()
	l, ok := c.chatLocks[chatID]
	if !ok {
		l = &sync.Mutex{}
		c.chatLocks[chatID] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (c *collabRuntime) statePath(chatID string) string {
	return filepath.Join(c.stateDir, chatID+".json")
}

// load returns the chat state; the caller must hold the chat lock.
func (c *collabRuntime) load(chatID string) *collabChatState {
	c.mu.Lock()
	st, ok := c.chats[chatID]
	c.mu.Unlock()
	if ok {
		return st
	}
	st = &collabChatState{}
	if c.stateDir != "" {
		if data, err := os.ReadFile(c.statePath(chatID)); err == nil {
			if err := json.Unmarshal(data, st); err != nil {
				slog.Warn("feishu collab: ignore corrupt state file", "chat_id", chatID, "error", err)
				st = &collabChatState{}
			}
		}
	}
	if st.Roster == nil {
		st.Roster = make(map[string]*collabRosterEntry)
	}
	if st.MentionNames == nil {
		st.MentionNames = make(map[string]string)
	}
	c.mu.Lock()
	c.chats[chatID] = st
	c.mu.Unlock()
	return st
}

// save persists the chat state; the caller must hold the chat lock.
func (c *collabRuntime) save(chatID string, st *collabChatState) {
	if c.stateDir == "" {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		slog.Warn("feishu collab: marshal state", "chat_id", chatID, "error", err)
		return
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		slog.Warn("feishu collab: create state dir", "dir", c.stateDir, "error", err)
		return
	}
	tmp := c.statePath(chatID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("feishu collab: write state", "chat_id", chatID, "error", err)
		return
	}
	if err := os.Rename(tmp, c.statePath(chatID)); err != nil {
		slog.Warn("feishu collab: rename state", "chat_id", chatID, "error", err)
	}
}

// collabSelfName returns this bot's display name from /bot/v3/info (cached).
func (p *Platform) collabSelfName(ctx context.Context) string {
	p.collab.mu.Lock()
	name := p.collab.selfName
	p.collab.mu.Unlock()
	if name != "" {
		return name
	}
	raw, err := p.collabGet(ctx, "/open-apis/bot/v3/info")
	if err != nil {
		slog.Warn(p.tag()+": collab fetch bot info", "error", err)
		return "机器人"
	}
	var result struct {
		Bot struct {
			AppName string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Bot.AppName == "" {
		slog.Warn(p.tag()+": collab parse bot info", "error", err)
		return "机器人"
	}
	p.collab.mu.Lock()
	p.collab.selfName = result.Bot.AppName
	p.collab.mu.Unlock()
	return result.Bot.AppName
}

// collabOwnerID returns the bot owner's open_id: the collab_owner option, or
// the app owner from the application API (needs application:application:
// self_manage, which QR-created apps have). Empty when it cannot be found,
// which keeps owner-only actions locked rather than open.
func (p *Platform) collabOwnerID(ctx context.Context) string {
	p.collab.mu.Lock()
	id := p.collab.ownerID
	p.collab.mu.Unlock()
	if id != "" {
		return id
	}
	raw, err := p.collabGet(ctx, "/open-apis/application/v6/applications/"+url.PathEscape(p.appID)+"?lang=zh_cn&user_id_type=open_id")
	if err != nil {
		slog.Warn(p.tag()+": collab owner lookup failed; owner-only actions stay locked (set collab_owner)", "error", err)
		return ""
	}
	var result struct {
		Data struct {
			App struct {
				Owner struct {
					OwnerID string `json:"owner_id"`
				} `json:"owner"`
			} `json:"app"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Data.App.Owner.OwnerID == "" {
		slog.Warn(p.tag()+": collab owner not found in app info; owner-only actions stay locked (set collab_owner)", "error", err)
		return ""
	}
	p.collab.mu.Lock()
	p.collab.ownerID = result.Data.App.Owner.OwnerID
	p.collab.mu.Unlock()
	return result.Data.App.Owner.OwnerID
}

func (p *Platform) collabIsOwner(ctx context.Context, openID string) bool {
	owner := p.collabOwnerID(ctx)
	return owner != "" && openID == owner
}

const collabOwnerOnlyNotice = "⛔ 只有我的主人能对我用命令（/list、/fork、/role 等），有事直接跟我说就行。"

// collabIsCommand reports whether text runs as a command instead of reaching
// the agent: "/name ..." (built-ins such as /list and /fork, /role, skills) or
// the "!" shell shortcut. Paths such as "/api/v1/users" are not commands.
func collabIsCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	first := fields[0]
	return strings.HasPrefix(first, "!") || strings.HasPrefix(first, "/") && !strings.Contains(first[1:], "/")
}

// collabRestrict applies the owner-only rules to a message about to reach the
// engine, in groups and private chats alike: anyone may talk to the agent, but
// only the bot's owner may run commands or answer its permission prompts and
// questions, so nobody else can rebind, reconfigure or approve for someone's
// bot. It returns true when the message is dropped.
func (p *Platform) collabRestrict(msg *core.Message) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p.collabIsOwner(ctx, msg.UserID) {
		return false
	}
	msg.CannotApprove = true
	if !collabIsCommand(msg.Content) {
		return false
	}
	slog.Info(p.tag()+": collab command from non-owner rejected", "session", msg.SessionKey, "user", msg.UserID)
	if err := p.Reply(ctx, msg.ReplyCtx, collabOwnerOnlyNotice); err != nil {
		slog.Warn(p.tag()+": collab owner-only notice failed", "error", err)
	}
	return true
}

// collabGet calls a GET Open API and returns the body. Like the platform's
// own calls it retries once with a fresh tenant token when the cached token
// is rejected (code 99991663), which otherwise silently empties the context.
func (p *Platform) collabGet(ctx context.Context, path string) ([]byte, error) {
	var raw []byte
	err := p.withFreshTenantAccessTokenRetry(ctx, "collab get", func(client *lark.Client, options ...larkcore.RequestOptionFunc) error {
		resp, err := client.Get(ctx, path, nil, larkcore.AccessTokenTypeTenant, options...)
		if err != nil {
			return err
		}
		var head struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(resp.RawBody, &head); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
		if head.Code != 0 {
			return fmt.Errorf("code=%d msg=%s", head.Code, head.Msg)
		}
		raw = resp.RawBody
		return nil
	})
	return raw, err
}

// collabFetchMessages pulls group messages created after sinceMs. With
// sinceMs == 0 it returns the latest `latest` messages instead. Messages are
// returned oldest first.
func (p *Platform) collabFetchMessages(ctx context.Context, chatID string, sinceMs int64, latest int) ([]collabMsg, error) {
	sortType := "ByCreateTimeAsc"
	limit := collabMaxIncrementalFetch
	if sinceMs == 0 {
		sortType = "ByCreateTimeDesc"
		limit = latest
	}
	var out []collabMsg
	pageToken := ""
	for len(out) < limit {
		q := url.Values{}
		q.Set("container_id_type", "chat")
		q.Set("container_id", chatID)
		q.Set("sort_type", sortType)
		q.Set("page_size", strconv.Itoa(collabPageSize))
		q.Set("card_msg_content_type", "user_card_content")
		if sinceMs > 0 {
			q.Set("start_time", strconv.FormatInt(sinceMs/1000, 10))
		}
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		raw, err := p.collabGet(ctx, "/open-apis/im/v1/messages?"+q.Encode())
		if err != nil {
			return out, fmt.Errorf("%s: collab list messages: %w", p.tag(), err)
		}
		page, hasMore, next, err := parseCollabMessagePage(raw)
		if err != nil {
			return out, fmt.Errorf("%s: collab list messages: %w", p.tag(), err)
		}
		for _, m := range page {
			if sinceMs > 0 && m.CreateMs <= sinceMs {
				continue
			}
			out = append(out, m)
		}
		if !hasMore || next == "" {
			break
		}
		pageToken = next
	}
	if len(out) > limit {
		out = out[:limit]
	}
	if sortType == "ByCreateTimeDesc" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

func parseCollabMessagePage(raw []byte) (msgs []collabMsg, hasMore bool, pageToken string, err error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
			Items     []struct {
				MessageID  string `json:"message_id"`
				MsgType    string `json:"msg_type"`
				CreateTime string `json:"create_time"`
				Deleted    bool   `json:"deleted"`
				Sender     struct {
					ID         string `json:"id"`
					SenderType string `json:"sender_type"`
				} `json:"sender"`
				Body struct {
					Content string `json:"content"`
				} `json:"body"`
				Mentions []collabMention `json:"mentions"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false, "", fmt.Errorf("parse: %w", err)
	}
	if resp.Code != 0 {
		return nil, false, "", fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	for _, it := range resp.Data.Items {
		ms, _ := strconv.ParseInt(it.CreateTime, 10, 64)
		msgs = append(msgs, collabMsg{
			ID:         it.MessageID,
			MsgType:    it.MsgType,
			CreateMs:   ms,
			SenderType: it.Sender.SenderType,
			SenderID:   it.Sender.ID,
			Content:    it.Body.Content,
			Mentions:   it.Mentions,
			Deleted:    it.Deleted,
		})
	}
	return msgs, resp.Data.HasMore, resp.Data.PageToken, nil
}

var (
	// Card mention tag; group 1 is the id, group 2 the optional mention_key
	// that receive events carry (e.g. @_user_1).
	collabCardAtRe     = regexp.MustCompile(`<at\s+id=["']?([^"'\s>]+)["']?(?:\s+mention_key=["']?([^"'\s>]+)["']?)?[^>]*>\s*</at>`)
	collabMentionKeyRe = regexp.MustCompile(`@_user_\d+`)
	collabCodeRe       = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")
)

// collabMentionName maps a mention to a readable name; self mentions become 你.
func collabMentionName(m collabMention, selfOpenID string, st *collabChatState) string {
	if m.ID != "" && m.ID == selfOpenID {
		return "你"
	}
	if m.Name != "" {
		return m.Name
	}
	if n := st.MentionNames[m.ID]; n != "" {
		return n
	}
	return "某人"
}

// collabMessageText renders one history message as plain text for the agent.
func collabMessageText(m collabMsg, selfOpenID string, st *collabChatState) string {
	switch m.MsgType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(m.Content), &body); err != nil {
			return "[文本]"
		}
		byKey := make(map[string]string, len(m.Mentions))
		for _, mt := range m.Mentions {
			byKey[mt.Key] = collabMentionName(mt, selfOpenID, st)
		}
		return collabMentionKeyRe.ReplaceAllStringFunc(body.Text, func(key string) string {
			if name, ok := byKey[key]; ok {
				return "@" + name
			}
			return key
		})
	case "post":
		text := extractPostPlainText(m.Content)
		byKey := make(map[string]string, len(m.Mentions))
		for _, mt := range m.Mentions {
			byKey[mt.Key] = collabMentionName(mt, selfOpenID, st)
		}
		return collabMentionKeyRe.ReplaceAllStringFunc(text, func(key string) string {
			if name, ok := byKey[key]; ok {
				return "@" + name
			}
			return key
		})
	case "interactive":
		return collabReplaceCardAts(extractInteractiveCardText(m.Content), m.Mentions, selfOpenID, st)
	case "image":
		return "[图片]"
	case "file":
		return "[文件]"
	case "audio":
		return "[语音]"
	case "media":
		return "[视频]"
	case "sticker":
		return "[表情]"
	default:
		return "[" + m.MsgType + "]"
	}
}

// collabReplaceCardAts turns <at id=...></at> tags in card text into @names.
// The ids inside the raw card are written by the sender and may belong to the
// sender app's view, so tags are matched to the message mentions by order.
func collabReplaceCardAts(text string, mentions []collabMention, selfOpenID string, st *collabChatState) string {
	i := 0
	return collabCardAtRe.ReplaceAllStringFunc(text, func(tag string) string {
		name := ""
		if i < len(mentions) {
			name = collabMentionName(mentions[i], selfOpenID, st)
		}
		i++
		if name == "" || name == "某人" {
			if sub := collabCardAtRe.FindStringSubmatch(tag); len(sub) > 1 && sub[1] == selfOpenID {
				name = "你"
			} else if name == "" {
				name = "某人"
			}
		}
		return "@" + name
	})
}

// parseCollabIntro reads an intro card posted by /role (see
// collabIntroMarkdown). The card mentions the owner first and the bot itself
// second, so the reader gets both ids in its own app's view from the
// message mentions; no machine-readable text is shown in the group.
func parseCollabIntro(m collabMsg, st *collabChatState) *collabRosterEntry {
	if m.MsgType != "interactive" {
		return nil
	}
	text := extractInteractiveCardText(m.Content)
	head := collabIntroHeadRe.FindStringSubmatch(text)
	if len(head) != 2 {
		return nil
	}
	entry := &collabRosterEntry{Name: strings.TrimSpace(head[1])}
	for line := range strings.SplitSeq(text, "\n") {
		if role, ok := strings.CutPrefix(strings.TrimSpace(line), collabIntroRolePrefix); ok {
			entry.Role = strings.TrimSpace(role)
		}
	}
	if len(m.Mentions) > 0 {
		owner := m.Mentions[0]
		entry.Owner = owner.Name
		if entry.Owner == "" {
			entry.Owner = st.MentionNames[owner.ID]
		}
	}
	if len(m.Mentions) > 1 {
		entry.OpenID = m.Mentions[1].ID
	}
	return entry
}

const collabIntroRolePrefix = "定位："

var collabIntroHeadRe = regexp.MustCompile(`\*\*🤖\s*(.+?)\s*的定位\*\*`)

func collabTruncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…（已截断）"
}

// collabTrigger describes the message that mentioned the bot.
type collabTrigger struct {
	id       string
	userID   string
	createMs int64
	fromBot  bool
}

// collabLoopState counts consecutive bot messages, newest first, back to the
// last human message, which also names the asker. msgs are oldest first and
// end at about the trigger; intro cards and system notices do not count.
func collabLoopState(msgs []collabMsg, trig collabTrigger) (botTurns int, askerID string) {
	sawTrigger := false
	empty := &collabChatState{}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.ID == trig.id {
			sawTrigger = true
		}
		if collabSkipMessage(m) {
			continue
		}
		if m.SenderType == "user" {
			askerID = m.SenderID
			break
		}
		if parseCollabIntro(m, empty) == nil {
			botTurns++
		}
	}
	// The history API may lag behind the receive event; count the trigger anyway.
	if !sawTrigger {
		if !trig.fromBot {
			return 0, trig.userID
		}
		botTurns++
	}
	return botTurns, askerID
}

// collabLoopWindow is how many recent messages are enough to count the loop.
func collabLoopWindow(maxBotTurns int) int {
	return min(max(maxBotTurns+5, 20), collabPageSize)
}

// AgentContext implements core.AgentContextProvider: in collab mode it puts
// the group messages the agent has not seen yet in front of the message.
func (p *Platform) AgentContext(replyCtx any, conversationID string) string {
	rc, ok := replyCtx.(replyContext)
	if !ok || p.collab == nil || !rc.collab {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.collabContextFor(ctx, rc, conversationID)
}

// collabContextFor pulls the group messages this conversation has not seen
// and builds the text put in front of the triggering message. A new
// conversation (conversationID changed) catches up from the latest messages.
func (p *Platform) collabContextFor(ctx context.Context, rc replyContext, conversationID string) string {
	c := p.collab
	unlock := c.lockChat(rc.chatID)
	defer unlock()
	st := c.load(rc.chatID)
	prevConversation, prevWatermark := st.ConversationID, st.WatermarkMs
	collabEnterConversation(st, conversationID)
	selfName := p.collabSelfName(ctx)

	incremental := st.WatermarkMs > 0
	msgs, err := p.collabFetchMessages(ctx, rc.chatID, st.WatermarkMs, c.initialHistory)
	if err != nil {
		slog.Warn(p.tag()+": collab history pull failed; replying without it", "chat_id", rc.chatID, "error", err)
		// Keep the old position so the next turn catches up again.
		st.ConversationID, st.WatermarkMs = prevConversation, prevWatermark
		return collabContext(selfName, p.resolveChatName(rc.chatID), st, nil, rc.collabBotTurns, c.maxBotTurns, p.appID, incremental)
	}
	humanNames := make(map[string]string)
	for name, id := range p.collabMembers(ctx, rc.chatID) {
		if id != "" {
			humanNames[id] = name
		}
	}
	lines := collabScan(st, msgs, rc.messageID, p.getBotOpenID(), p.appID, incremental, humanNames)
	if rc.collabTriggerMs > st.WatermarkMs {
		st.WatermarkMs = rc.collabTriggerMs
	}
	c.save(rc.chatID, st)
	return collabContext(selfName, p.resolveChatName(rc.chatID), st, lines, rc.collabBotTurns, c.maxBotTurns, p.appID, incremental)
}

// collabEnterConversation points the chat state at conversationID. A new
// conversation (/new, /switch, /fork, idle rotation) has seen none of the
// group yet, so it reads the latest messages from scratch.
func collabEnterConversation(st *collabChatState, conversationID string) {
	if st.ConversationID != conversationID {
		st.ConversationID = conversationID
		st.WatermarkMs = 0
	}
}

// collabScan walks pulled messages oldest first: it learns mention names and
// roster entries, advances the watermark, and returns the transcript lines
// shown to the agent. The trigger itself is left out; it is the agent's input.
func collabScan(st *collabChatState, msgs []collabMsg, triggerID, selfOpenID, selfAppID string, incremental bool, humanNames map[string]string) []string {
	// First pass: learn names and intros, so lines written before a bot
	// introduced itself still show its name.
	for _, m := range msgs {
		if m.CreateMs > st.WatermarkMs {
			st.WatermarkMs = m.CreateMs
		}
		if collabSkipMessage(m) {
			continue
		}
		for _, mt := range m.Mentions {
			if mt.ID != "" && mt.Name != "" {
				st.MentionNames[mt.ID] = mt.Name
			}
		}
		if m.SenderType != "app" {
			continue
		}
		if entry := parseCollabIntro(m, st); entry != nil {
			entry.AppID = m.SenderID
			st.Roster[m.SenderID] = entry
			if entry.OpenID != "" && entry.Name != "" {
				st.MentionNames[entry.OpenID] = entry.Name // card mentions of bots carry no name
			}
		}
	}

	var lines []string
	for _, m := range msgs {
		if collabSkipMessage(m) || m.ID == triggerID {
			continue
		}
		isBot := m.SenderType == "app"
		if isBot && m.SenderID == selfAppID && incremental {
			continue // our own replies are already in the agent session
		}
		lines = append(lines, collabTranscriptLine(m, collabMessageText(m, selfOpenID, st), isBot, selfAppID, humanNames, st))
	}
	return lines
}

// collabSkipMessage drops deleted messages and system notices (group created,
// member added, ...), which carry no sender and must not reset the loop counter.
func collabSkipMessage(m collabMsg) bool {
	return m.Deleted || m.MsgType == "system" || (m.SenderType != "user" && m.SenderType != "app")
}

func collabTranscriptLine(m collabMsg, text string, isBot bool, selfAppID string, humanNames map[string]string, st *collabChatState) string {
	who := ""
	switch {
	case isBot && m.SenderID == selfAppID:
		who = "你（之前的发言）"
	case isBot:
		if e := st.Roster[m.SenderID]; e != nil && e.Name != "" {
			who = e.Name + "（机器人）"
		} else {
			who = "未登记的机器人"
		}
	default:
		who = humanNames[m.SenderID]
		if who == "" {
			who = "群成员"
		}
		who += "（人）"
	}
	ts := time.UnixMilli(m.CreateMs).Format("01-02 15:04")
	return fmt.Sprintf("[%s] %s：%s", ts, who, collabTruncate(strings.TrimSpace(text), collabMaxMessageChars))
}

// collabContext builds the text prepended to the agent's input.
func collabContext(selfName, chatName string, st *collabChatState, lines []string, botTurns, maxBotTurns int, selfAppID string, incremental bool) string {
	var b strings.Builder
	group := "飞书群"
	if chatName != "" {
		group = "飞书群「" + chatName + "」"
	}
	owner := st.Owner
	if owner == "" {
		owner = "未设置"
	}
	role := st.Role
	if role == "" {
		role = "未设置（主人可以发 /role 设置）"
	}
	fmt.Fprintf(&b, "[群协作] 你是%s里的 AI 助手「%s」，主人：%s，定位：%s。\n", group, selfName, owner, role)

	var peers []*collabRosterEntry
	for appID, e := range st.Roster {
		if appID != selfAppID {
			peers = append(peers, e)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	b.WriteString("群里登记过的其他 AI 助手：\n")
	if len(peers) == 0 {
		b.WriteString("- （暂无，其他助手的主人发 /role 后才会出现）\n")
	}
	for _, e := range peers {
		fmt.Fprintf(&b, "- %s（主人：%s；定位：%s）\n", e.Name, e.Owner, e.Role)
	}
	b.WriteString("发言规则：\n")
	b.WriteString("1. 先看下面的群消息，再回复最后这条 @ 你的消息；用 Markdown 写。\n")
	b.WriteString("2. 需要别人参与时，在回复末尾同时 @人名 和 @他的 AI 助手名（只能用群成员和上面列出的名字，可以同时找几个人）。\n")
	b.WriteString("3. 讨论有结论或需要人拍板时，只 @ 提问的人做总结，不要再 @ 任何 AI 助手。\n")
	b.WriteString("4. 不要在群里贴密钥、密码、token 等敏感信息。\n")
	fmt.Fprintf(&b, "当前 AI 助手之间已连续发言 %d 次（上限 %d 次，到上限后不能再 @ AI 助手）。\n\n", botTurns, maxBotTurns)

	if incremental {
		fmt.Fprintf(&b, "[你上次发言之后的群消息，共 %d 条]\n", len(lines))
	} else {
		fmt.Fprintf(&b, "[最近的群消息，共 %d 条]\n", len(lines))
	}
	if len(lines) == 0 {
		b.WriteString("（没有新消息）\n")
	}
	// Keep the newest lines when the transcript is too long.
	total := 0
	start := len(lines)
	for start > 0 && total+len(lines[start-1]) <= collabMaxTranscriptChars {
		start--
		total += len(lines[start]) + 1
	}
	if start > 0 {
		fmt.Fprintf(&b, "（更早的 %d 条太长，已省略）\n", start)
	}
	for _, l := range lines[start:] {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("\n[@ 你的消息]")
	return b.String()
}

// collabInboundText extracts the user text of a group message that mentioned
// the bot. ok is false for message types collab does not handle.
func (p *Platform) collabInboundText(msgType, content string, mentions []*larkim.MentionEvent) (string, bool) {
	selfOpenID := p.getBotOpenID()
	switch msgType {
	case "text":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &body); err != nil {
			return "", false
		}
		return stripMentions(body.Text, mentions, selfOpenID), true
	case "post":
		return strings.TrimSpace(stripMentions(extractPostPlainText(content), mentions, selfOpenID)), true
	case "interactive":
		return collabEventCardText(content, mentions, selfOpenID), true
	default:
		return "", false
	}
}

// collabEventCardText reads a card from a receive event: the original card is
// in the user_dsl field, and its <at> tags carry mention_key attributes.
func collabEventCardText(content string, mentions []*larkim.MentionEvent, selfOpenID string) string {
	var wrapper struct {
		UserDSL string `json:"user_dsl"`
	}
	cardJSON := content
	if json.Unmarshal([]byte(content), &wrapper) == nil && wrapper.UserDSL != "" {
		cardJSON = wrapper.UserDSL
	}
	text := extractInteractiveCardText(cardJSON)
	names := make([]string, 0, len(mentions))
	byKey := make(map[string]string, len(mentions))
	for _, m := range mentions {
		name := ""
		if m.Name != nil {
			name = *m.Name
		}
		if m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == selfOpenID {
			name = "" // drop our own mention, like stripMentions does for text
		} else if name == "" {
			name = "某人"
		}
		names = append(names, name)
		if m.Key != nil {
			byKey[*m.Key] = name
		}
	}
	i := 0
	text = collabCardAtRe.ReplaceAllStringFunc(text, func(tag string) string {
		name := "某人"
		if sub := collabCardAtRe.FindStringSubmatch(tag); len(sub) == 3 && sub[2] != "" {
			if n, ok := byKey[sub[2]]; ok {
				name = n
			}
		} else if i < len(names) {
			name = names[i]
		}
		i++
		if name == "" {
			return ""
		}
		return "@" + name
	})
	return strings.TrimSpace(text)
}

// collabDispatch handles a group message that mentioned the bot in collab mode.
func (p *Platform) collabDispatch(ctx context.Context, msgType, content string, mentions []*larkim.MentionEvent, messageID, sessionKey, userID, senderType, chatID string, rctx replyContext, createTimeMs int64) {
	fromBot := senderType != "" && senderType != "user"

	text, ok := p.collabInboundText(msgType, content, mentions)
	if !ok {
		// Images, files and other types keep the regular handling.
		p.dispatchMessageWithHistory(ctx, msgType, content, mentions, messageID, sessionKey, userID, chatID, rctx, "", createTimeMs, groupHistoryContext{})
		return
	}
	trimmed := strings.TrimSpace(text)

	// Other commands are checked in dispatchCoreMessage (collabRestrict).
	if role, ok := strings.CutPrefix(trimmed, "/role"); ok {
		if p.collabIsOwner(ctx, userID) {
			p.collabHandleRole(ctx, chatID, userID, strings.TrimSpace(role), rctx)
		} else {
			p.collabSendCard(ctx, rctx, collabOwnerOnlyNotice)
		}
		return
	}

	// Count the bot-to-bot chain now; the group history itself is read when
	// the message reaches the agent (AgentContext), so it is never stale.
	recent, err := p.collabFetchMessages(ctx, chatID, 0, collabLoopWindow(p.collab.maxBotTurns))
	if err != nil {
		slog.Warn(p.tag()+": collab loop check failed; treating the chain as new", "chat_id", chatID, "error", err)
	}
	botTurns, askerID := collabLoopState(recent, collabTrigger{id: messageID, userID: userID, createMs: createTimeMs, fromBot: fromBot})
	if fromBot && botTurns >= p.collab.maxBotTurns {
		slog.Info(p.tag()+": collab loop limit reached; ignoring bot mention", "chat_id", chatID, "message_id", messageID, "bot_turns", botTurns)
		return
	}
	rctx.collab = true
	rctx.collabBotTurns = botTurns
	rctx.collabAskerID = askerID
	rctx.collabTriggerMs = createTimeMs

	userName := ""
	if !fromBot && userID != "" {
		userName = p.collabHumanName(ctx, chatID, userID)
	}
	p.dispatchCoreMessage(&core.Message{
		SessionKey: sessionKey, Platform: p.platformName, MessageID: messageID,
		UserID: userID, UserName: userName, ChatName: p.resolveChatName(chatID),
		Content: trimmed, ReplyCtx: rctx, UserMessageTimeMs: createTimeMs,
	})
}

// collabHumanName looks a person up in the group member list (needs only
// im:chat.members:read) and falls back to the contact API.
func (p *Platform) collabHumanName(ctx context.Context, chatID, openID string) string {
	for name, id := range p.collabMembers(ctx, chatID) {
		if id == openID {
			return name
		}
	}
	return p.resolveUserName(openID)
}

const collabMembersTTL = 10 * time.Minute

type collabMembersEntry struct {
	names     map[string]string // display name -> open_id
	fetchedAt time.Time
}

// collabMembers lists the people in a chat (name -> open_id), cached briefly.
// It uses the same plain GET as the history pull; the SDK iterator used by
// getChatMembers can stall until its 10s deadline.
func (p *Platform) collabMembers(ctx context.Context, chatID string) map[string]string {
	if v, ok := p.collab.members.Load(chatID); ok {
		if e := v.(*collabMembersEntry); time.Since(e.fetchedAt) < collabMembersTTL {
			return e.names
		}
	}
	names := make(map[string]string)
	pageToken := ""
	for range 20 {
		q := url.Values{}
		q.Set("member_id_type", "open_id")
		q.Set("page_size", "100")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		raw, err := p.collabGet(ctx, "/open-apis/im/v1/chats/"+chatID+"/members?"+q.Encode())
		if err != nil {
			slog.Warn(p.tag()+": collab list members", "chat_id", chatID, "error", err)
			return names // not cached, so the next call retries
		}
		var result struct {
			Data struct {
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
				Items     []struct {
					MemberID string `json:"member_id"`
					Name     string `json:"name"`
				} `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			slog.Warn(p.tag()+": collab parse members", "chat_id", chatID, "error", err)
			return names
		}
		for _, it := range result.Data.Items {
			if it.Name != "" && it.MemberID != "" {
				names[it.Name] = it.MemberID
			}
		}
		if !result.Data.HasMore || result.Data.PageToken == "" {
			break
		}
		pageToken = result.Data.PageToken
	}
	p.collab.members.Store(chatID, &collabMembersEntry{names: names, fetchedAt: time.Now()})
	return names
}

// collabHandleRole stores this bot's role for the chat and posts an intro card
// that other bots read to build their roster.
func (p *Platform) collabHandleRole(ctx context.Context, chatID, userID, role string, rctx replyContext) {
	selfName := p.collabSelfName(ctx)
	if role == "" {
		unlock := p.collab.lockChat(chatID)
		current := p.collab.load(chatID).Role
		unlock()
		if current == "" {
			current = "还没设置"
		}
		msg := fmt.Sprintf("**%s 的定位**：%s\n\n设置方法：@%s /role 一句话说明我负责什么", selfName, current, selfName)
		p.collabSendCard(ctx, rctx, msg)
		return
	}
	role = strings.Join(strings.Fields(role), " ") // one line, so the intro parser keeps all of it
	owner := p.collabHumanName(ctx, chatID, userID)
	unlock := p.collab.lockChat(chatID)
	st := p.collab.load(chatID)
	st.Role = role
	st.Owner = owner
	st.Roster[p.appID] = &collabRosterEntry{Name: selfName, AppID: p.appID, OpenID: p.getBotOpenID(), Owner: owner, Role: role}
	p.collab.save(chatID, st)
	unlock()
	p.collabSendCard(ctx, rctx, collabIntroMarkdown(selfName, userID, p.getBotOpenID(), role))
}

// collabIntroMarkdown builds the /role intro card. It must mention the owner
// first and the bot itself second; parseCollabIntro relies on that order.
func collabIntroMarkdown(name, ownerOpenID, selfOpenID, role string) string {
	return fmt.Sprintf("**🤖 %s 的定位**\n主人：<at id=%s></at>\n%s%s\n找我：<at id=%s></at>（建议同时 @ 我的主人）",
		name, ownerOpenID, collabIntroRolePrefix, role, selfOpenID)
}

func (p *Platform) collabSendCard(ctx context.Context, rc replyContext, markdown string) {
	body := buildCardJSON(markdown)
	var err error
	if p.shouldUseThreadOrReplyAPI(rc) {
		err = p.replyMessage(ctx, rc, larkim.MsgTypeInteractive, body)
	} else {
		err = p.sendNewMessageToChat(ctx, rc, larkim.MsgTypeInteractive, body)
	}
	if err != nil {
		slog.Warn(p.tag()+": collab send card failed", "chat_id", rc.chatID, "error", err)
	}
}

// collabMentionTarget is someone the agent may @ in its reply.
type collabMentionTarget struct {
	openID string
	isBot  bool
	owner  string // for bots: the owner's display name from the roster
}

// collabSend renders an agent reply as a Markdown card, turning @names into
// card mentions and applying the loop guard.
func (p *Platform) collabSend(ctx context.Context, rc replyContext, content string) error {
	targets := p.collabTargets(ctx, rc.chatID)
	content = collabRenderReply(content, targets, rc.collabBotTurns, p.collab.maxBotTurns, rc.collabAskerID)
	body := buildCardJSON(content)
	if p.shouldUseThreadOrReplyAPI(rc) {
		return p.replyMessage(ctx, rc, larkim.MsgTypeInteractive, body)
	}
	return p.sendNewMessageToChat(ctx, rc, larkim.MsgTypeInteractive, body)
}

// collabTargets merges group members (people) and registered bots by name.
func (p *Platform) collabTargets(ctx context.Context, chatID string) map[string]collabMentionTarget {
	targets := make(map[string]collabMentionTarget)
	for name, id := range p.collabMembers(ctx, chatID) {
		if id != "" {
			targets[name] = collabMentionTarget{openID: id}
		}
	}
	p.mu.RLock()
	for name, id := range p.mentionMap {
		if id != "" {
			targets[name] = collabMentionTarget{openID: id, isBot: true}
		}
	}
	p.mu.RUnlock()
	unlock := p.collab.lockChat(chatID)
	st := p.collab.load(chatID)
	for appID, e := range st.Roster {
		if appID != p.appID && e.Name != "" && e.OpenID != "" {
			targets[e.Name] = collabMentionTarget{openID: e.OpenID, isBot: true, owner: e.Owner}
		}
	}
	unlock()
	return targets
}

// collabRenderReply replaces @name with card <at> tags. A mentioned bot's
// owner is mentioned too when the reply does not already do so. Once the next
// bot message would reach the loop limit, bot mentions are dropped and the
// last human speaker is asked to decide instead.
func collabRenderReply(content string, targets map[string]collabMentionTarget, botTurns, maxBotTurns int, askerID string) string {
	if !strings.Contains(content, "@") || len(targets) == 0 {
		return content
	}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len([]rune(names[i])) > len([]rune(names[j])) })

	// Code spans and blocks are literal text (e.g. an agent quoting `@name`
	// syntax); mask them so their @ never becomes a real mention.
	var codes []string
	content = collabCodeRe.ReplaceAllStringFunc(content, func(code string) string {
		codes = append(codes, code)
		return fmt.Sprintf("\x00c%d\x00", len(codes)-1)
	})
	original := content
	limitReached := botTurns+1 >= maxBotTurns
	droppedBots := false
	addedOwners := make(map[string]bool)
	// Replace longest names first; placeholders keep shorter names from
	// matching inside an already-replaced tag.
	placeholders := make(map[string]string)
	for i, name := range names {
		pattern := "@" + name
		if !strings.Contains(content, pattern) {
			continue
		}
		t := targets[name]
		ph := fmt.Sprintf("\x00%d\x00", i)
		if t.isBot && limitReached {
			placeholders[ph] = name
			droppedBots = true
			content = strings.ReplaceAll(content, pattern, ph)
			continue
		}
		placeholders[ph] = fmt.Sprintf("<at id=%s></at>", t.openID)
		if owner, ok := targets[t.owner]; t.isBot && ok && !owner.isBot &&
			!addedOwners[t.owner] && !strings.Contains(original, "@"+t.owner) {
			addedOwners[t.owner] = true
			withOwner := fmt.Sprintf("\x00o%d\x00", i)
			placeholders[withOwner] = fmt.Sprintf("<at id=%s></at> <at id=%s></at>", owner.openID, t.openID)
			content = strings.Replace(content, pattern, withOwner, 1)
		}
		content = strings.ReplaceAll(content, pattern, ph)
	}
	for ph, repl := range placeholders {
		content = strings.ReplaceAll(content, ph, repl)
	}
	for i, code := range codes {
		content = strings.Replace(content, fmt.Sprintf("\x00c%d\x00", i), code, 1)
	}
	if droppedBots {
		// No "> " quote here: Feishu drops the blank line before it in cards.
		note := fmt.Sprintf("\n\n⏸ **AI 助手之间已连续发言 %d 次，先暂停自动交接。**", botTurns+1)
		if askerID != "" {
			note += fmt.Sprintf("<at id=%s></at> 请看一下要不要继续。", askerID)
		}
		content += note
	}
	return content
}
