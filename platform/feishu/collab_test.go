package feishu

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/chenhg5/cc-connect/core"
)

func newTestCollabState() *collabChatState {
	return &collabChatState{
		Roster:       map[string]*collabRosterEntry{},
		MentionNames: map[string]string{},
	}
}

func TestNewCollabRuntime(t *testing.T) {
	if c := newCollabRuntime(map[string]any{}); c != nil {
		t.Fatalf("collab should be off by default")
	}
	c := newCollabRuntime(map[string]any{
		"collab": true, "collab_initial_history": int64(50), "collab_max_bot_turns": int64(3),
		"cc_data_dir": "/tmp/x", "cc_project": "p1",
	})
	if c == nil || c.initialHistory != 50 || c.maxBotTurns != 3 {
		t.Fatalf("unexpected runtime: %+v", c)
	}
	if !strings.HasSuffix(c.stateDir, "feishu-collab/p1") {
		t.Fatalf("state dir = %q", c.stateDir)
	}
	d := newCollabRuntime(map[string]any{"collab": true})
	if d.initialHistory != collabDefaultInitialHistory || d.maxBotTurns != collabDefaultMaxBotTurns || d.stateDir != "" {
		t.Fatalf("defaults not applied: %+v", d)
	}
}

func TestCollabStateSaveLoad(t *testing.T) {
	c := newCollabRuntime(map[string]any{"collab": true, "cc_data_dir": t.TempDir()})
	st := c.load("oc_1")
	st.WatermarkMs = 42
	st.Roster["cli_b"] = &collabRosterEntry{Name: "Claude-B", OpenID: "ou_b"}
	c.save("oc_1", st)

	fresh := newCollabRuntime(map[string]any{"collab": true, "cc_data_dir": c.stateDir[:len(c.stateDir)-len("/feishu-collab/default")]})
	got := fresh.load("oc_1")
	if got.WatermarkMs != 42 || got.Roster["cli_b"] == nil || got.Roster["cli_b"].OpenID != "ou_b" {
		t.Fatalf("state not persisted: %+v", got)
	}
}

func TestCollabRenderReply_MentionsBecomeCardAts(t *testing.T) {
	targets := map[string]collabMentionTarget{
		"张三":       {openID: "ou_zhang"},
		"Claude-B": {openID: "ou_b", isBot: true},
	}
	got := collabRenderReply("方案见上。@张三 @Claude-B 请评估", targets, 0, 6, "ou_asker")
	want := "方案见上。<at id=ou_zhang></at> <at id=ou_b></at> 请评估"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCollabRenderReply_AddsOwnerOfMentionedBot(t *testing.T) {
	targets := map[string]collabMentionTarget{
		"张三":       {openID: "ou_zhang"},
		"Claude-B": {openID: "ou_b", isBot: true, owner: "张三"},
	}
	if got := collabRenderReply("@Claude-B 请看，@Claude-B 再补充", targets, 0, 6, ""); got != "<at id=ou_zhang></at> <at id=ou_b></at> 请看，<at id=ou_b></at> 再补充" {
		t.Fatalf("owner should be added once before the bot: %q", got)
	}
	if got := collabRenderReply("@张三 @Claude-B 请看", targets, 0, 6, ""); got != "<at id=ou_zhang></at> <at id=ou_b></at> 请看" {
		t.Fatalf("owner already mentioned must not repeat: %q", got)
	}
	if got := collabRenderReply("@Claude-B 请看", targets, 5, 6, "ou_asker"); strings.Contains(got, "ou_zhang") || strings.Contains(got, "ou_b") {
		t.Fatalf("at the loop limit neither the bot nor its owner is added: %q", got)
	}
}

func TestCollabRenderReply_IgnoresMentionsInCode(t *testing.T) {
	targets := map[string]collabMentionTarget{
		"张三":       {openID: "ou_zhang"},
		"Claude-B": {openID: "ou_b", isBot: true, owner: "张三"},
	}
	in := "我们写的 `@Claude-B` 是靠名字匹配：\n```\n@张三 @Claude-B\n```\n@Claude-B 请回应"
	want := "我们写的 `@Claude-B` 是靠名字匹配：\n```\n@张三 @Claude-B\n```\n<at id=ou_zhang></at> <at id=ou_b></at> 请回应"
	if got := collabRenderReply(in, targets, 0, 6, ""); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCollabRenderReply_LongestNameFirst(t *testing.T) {
	targets := map[string]collabMentionTarget{
		"Claude":   {openID: "ou_short", isBot: true},
		"Claude-B": {openID: "ou_long", isBot: true},
	}
	got := collabRenderReply("@Claude-B 和 @Claude", targets, 0, 6, "")
	if got != "<at id=ou_long></at> 和 <at id=ou_short></at>" {
		t.Fatalf("got %q", got)
	}
}

func TestCollabRenderReply_LoopLimitDropsBotMentions(t *testing.T) {
	targets := map[string]collabMentionTarget{
		"张三":       {openID: "ou_zhang"},
		"Claude-B": {openID: "ou_b", isBot: true},
	}
	// Five bot messages already; this reply would be the sixth (the limit).
	got := collabRenderReply("@张三 @Claude-B 继续", targets, 5, 6, "ou_asker")
	if strings.Contains(got, "ou_b") {
		t.Fatalf("bot mention should be dropped at the limit: %q", got)
	}
	if !strings.Contains(got, "<at id=ou_zhang></at>") || !strings.Contains(got, "Claude-B 继续") {
		t.Fatalf("human mention and bot name text should stay: %q", got)
	}
	if !strings.Contains(got, "已连续发言 6 次") || !strings.Contains(got, "<at id=ou_asker></at>") {
		t.Fatalf("loop note should ask the last human: %q", got)
	}
	// Below the limit nothing is dropped.
	if got := collabRenderReply("@Claude-B 继续", targets, 4, 6, "ou_asker"); got != "<at id=ou_b></at> 继续" {
		t.Fatalf("below limit got %q", got)
	}
}

// introCardMsg wraps an intro card the way the history API returns it: the
// mentions carry ids in the reader's view, and bot mentions have no name.
func introCardMsg(t *testing.T, id, senderAppID, markdown string, mentions []collabMention) collabMsg {
	t.Helper()
	card, err := json.Marshal(map[string]any{"schema": "2.0", "body": map[string]any{"elements": []map[string]any{{"tag": "markdown", "content": markdown}}}})
	if err != nil {
		t.Fatal(err)
	}
	return collabMsg{ID: id, MsgType: "interactive", SenderType: "app", SenderID: senderAppID, Content: string(card), Mentions: mentions}
}

func TestCollabIntroRoundTrip(t *testing.T) {
	md := collabIntroMarkdown("Claude-A", "ou_owner_sender_view", "ou_a_sender_view", "负责网关和运行时")
	if strings.Contains(md, "cli_") || !strings.Contains(md, "<at id=ou_owner_sender_view></at>") {
		t.Fatalf("intro should only show names via mentions: %q", md)
	}
	m := introCardMsg(t, "m1", "cli_a", md, []collabMention{
		{Key: "@_user_1", ID: "ou_owner_reader_view", Name: "李四"},
		{Key: "@_user_2", ID: "ou_a_reader_view"},
	})
	entry := parseCollabIntro(m, newTestCollabState())
	if entry == nil {
		t.Fatalf("intro not parsed: %q", md)
	}
	if entry.Name != "Claude-A" || entry.Owner != "李四" || entry.OpenID != "ou_a_reader_view" || entry.Role != "负责网关和运行时" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if parseCollabIntro(introCardMsg(t, "m2", "cli_a", "**结论**\n定位：不是自我介绍", nil), newTestCollabState()) != nil {
		t.Fatalf("a card without the intro header must not parse")
	}
}

func TestCollabMessageText_TextMentions(t *testing.T) {
	st := newTestCollabState()
	m := collabMsg{
		MsgType: "text",
		Content: `{"text":"@_user_1 @_user_2 看下这个"}`,
		Mentions: []collabMention{
			{Key: "@_user_1", ID: "ou_self", Name: "Claude-A"},
			{Key: "@_user_2", ID: "ou_zhang", Name: "张三"},
		},
	}
	if got := collabMessageText(m, "ou_self", st); got != "@你 @张三 看下这个" {
		t.Fatalf("got %q", got)
	}
}

func TestCollabMessageText_CardAtsByOrder(t *testing.T) {
	st := newTestCollabState()
	st.MentionNames["ou_b_seen_by_me"] = "Claude-B"
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{"elements": []map[string]any{{
			"tag": "markdown",
			// ids inside the raw card belong to the sender app's view.
			"content": "结论如下 <at id=ou_x_sender_view></at> <at id=ou_y_sender_view></at> 请确认",
		}}},
	}
	raw, _ := json.Marshal(card)
	m := collabMsg{
		MsgType: "interactive",
		Content: string(raw),
		Mentions: []collabMention{
			{Key: "@_user_1", ID: "ou_zhang", Name: "张三"},
			{Key: "@_user_2", ID: "ou_b_seen_by_me"}, // card mentions of bots carry no name
		},
	}
	if got := collabMessageText(m, "ou_self", st); got != "结论如下 @张三 @Claude-B 请确认" {
		t.Fatalf("got %q", got)
	}
}

func TestCollabEventCardText_UserDSLWithMentionKey(t *testing.T) {
	dsl := `{"body":{"elements":[{"tag":"markdown","content":"**验证** <at id=ou_x mention_key=@_user_2></at> 和 <at id=ou_y mention_key=@_user_1></at> 请看"}]}}`
	content, _ := json.Marshal(map[string]any{"title": nil, "elements": []any{}, "user_dsl": dsl})
	mentions := []*larkim.MentionEvent{
		{Key: strPtr("@_user_1"), Id: &larkim.UserId{OpenId: strPtr("ou_self")}, Name: strPtr("Claude-A")},
		{Key: strPtr("@_user_2"), Id: &larkim.UserId{OpenId: strPtr("ou_zhang")}, Name: strPtr("张三")},
	}
	got := collabEventCardText(string(content), mentions, "ou_self")
	if got != "**验证** @张三 和  请看" {
		t.Fatalf("got %q", got)
	}
}

func TestCollabScan_RosterAndTranscript(t *testing.T) {
	st := newTestCollabState()
	intro := introCardMsg(t, "m2", "cli_b", collabIntroMarkdown("Claude-B", "ou_zhang_b_view", "ou_b_self", "负责工具服务"),
		[]collabMention{{Key: "@_user_1", ID: "ou_zhang", Name: "张三"}, {Key: "@_user_2", ID: "ou_b"}})
	intro.CreateMs = 2000
	msgs := []collabMsg{
		{ID: "m0", MsgType: "text", CreateMs: 500, SenderType: "app", SenderID: "cli_b", Content: `{"text":"登记前的发言"}`},
		{ID: "m1", MsgType: "text", CreateMs: 1000, SenderType: "user", SenderID: "ou_li", Content: `{"text":"开始讨论"}`},
		intro,
		{ID: "m3", MsgType: "text", CreateMs: 3000, SenderType: "app", SenderID: "cli_a", Content: `{"text":"我先说"}`},
		{ID: "m4", MsgType: "text", CreateMs: 4000, SenderType: "app", SenderID: "cli_b", Content: `{"text":"我补充"}`},
		{ID: "m5", MsgType: "system", CreateMs: 4500, Content: `{"template":"{from_user} invited {to_chatters} to the group."}`},
		{ID: "m6", MsgType: "text", CreateMs: 5000, SenderType: "app", SenderID: "cli_b", Content: `{"text":"@_user_1 该你了"}`,
			Mentions: []collabMention{{Key: "@_user_1", ID: "ou_a"}}},
	}
	humans := map[string]string{"ou_li": "李四"}
	lines := collabScan(st, msgs, "m6", "ou_a", "cli_a", true, humans)

	if st.WatermarkMs != 5000 {
		t.Fatalf("watermark = %d", st.WatermarkMs)
	}
	if botTurns, asker := collabLoopState(msgs, collabTrigger{id: "m6", fromBot: true}); botTurns != 3 || asker != "ou_li" {
		t.Fatalf("loop state = %d %q; want 3 bot messages (m3, m4, m6; not the intro) and asker ou_li", botTurns, asker)
	}
	if e := st.Roster["cli_b"]; e == nil || e.Name != "Claude-B" || e.AppID != "cli_b" || e.OpenID != "ou_b" {
		t.Fatalf("roster not learned from intro: %+v", st.Roster)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "我先说") {
		t.Fatalf("own message should be skipped in incremental mode: %s", joined)
	}
	if strings.Contains(joined, "该你了") {
		t.Fatalf("trigger should not be in the transcript: %s", joined)
	}
	if !strings.Contains(joined, "李四（人）：开始讨论") || !strings.Contains(joined, "Claude-B（机器人）：我补充") {
		t.Fatalf("unexpected transcript: %s", joined)
	}
	if !strings.Contains(joined, "Claude-B（机器人）：登记前的发言") {
		t.Fatalf("messages before the intro should use the learned name: %s", joined)
	}
	if strings.Contains(joined, "invited") || strings.Contains(joined, "[system]") {
		t.Fatalf("system notices must be skipped: %s", joined)
	}
}

func TestCollabLoopState_TriggerMissingFromHistory(t *testing.T) {
	msgs := []collabMsg{
		{ID: "m1", MsgType: "text", SenderType: "user", SenderID: "ou_li", Content: `{"text":"开始"}`},
		{ID: "m2", MsgType: "text", SenderType: "app", SenderID: "cli_a", Content: `{"text":"一"}`},
	}
	// A human trigger the history API has not returned yet resets the chain.
	if n, asker := collabLoopState(msgs, collabTrigger{id: "mx", userID: "ou_zhang"}); n != 0 || asker != "ou_zhang" {
		t.Fatalf("human trigger: %d %q", n, asker)
	}
	// A bot trigger that is not returned yet still counts.
	if n, asker := collabLoopState(msgs, collabTrigger{id: "my", fromBot: true}); n != 2 || asker != "ou_li" {
		t.Fatalf("bot trigger: %d %q", n, asker)
	}
	if n, asker := collabLoopState(nil, collabTrigger{id: "mz", fromBot: true}); n != 1 || asker != "" {
		t.Fatalf("empty history: %d %q", n, asker)
	}
}

func TestCollabEnterConversation(t *testing.T) {
	st := newTestCollabState()
	st.ConversationID = "s1"
	st.WatermarkMs = 500
	collabEnterConversation(st, "s1")
	if st.WatermarkMs != 500 {
		t.Fatalf("the same conversation keeps reading incrementally: %d", st.WatermarkMs)
	}
	collabEnterConversation(st, "s2")
	if st.ConversationID != "s2" || st.WatermarkMs != 0 {
		t.Fatalf("a new conversation must re-read the group: %+v", st)
	}
}

func TestCollabScan_NewConversationIncludesOwnMessages(t *testing.T) {
	msgs := []collabMsg{
		{ID: "m1", MsgType: "text", CreateMs: 100, SenderType: "app", SenderID: "cli_a", Content: `{"text":"我之前说过"}`},
	}
	lines := collabScan(newTestCollabState(), msgs, "mx", "ou_a", "cli_a", false, nil)
	if len(lines) != 1 || !strings.Contains(lines[0], "你（之前的发言）：我之前说过") {
		t.Fatalf("a fresh conversation must see the bot's own earlier messages: %v", lines)
	}
}

func TestCollabContext_RosterRulesAndTruncation(t *testing.T) {
	st := newTestCollabState()
	st.Role = "负责后端"
	st.Owner = "李四"
	st.Roster["cli_a"] = &collabRosterEntry{Name: "Claude-A", Owner: "李四", Role: "负责后端"}
	st.Roster["cli_b"] = &collabRosterEntry{Name: "Claude-B", Owner: "张三", Role: "负责工具服务"}
	long := strings.Repeat("长", collabMaxTranscriptChars/3)
	lines := []string{"[01-01 10:00] 甲（人）：" + long, "[01-01 10:01] 乙（人）：" + long, "[01-01 10:02] 丙（人）：最新一条"}
	got := collabContext("Claude-A", "测试", st, lines, 2, 6, "cli_a", true)

	for _, want := range []string{"「Claude-A」", "群「测试」", "定位：负责后端", "Claude-B（主人：张三；定位：负责工具服务）", "已连续发言 2 次（上限 6 次", "丙（人）：最新一条", "已省略", "[@ 你的消息]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("context missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- Claude-A（") {
		t.Fatalf("self must not be listed as a peer:\n%s", got)
	}
}

func TestCollabCardKeepsAtTags(t *testing.T) {
	body := buildCardJSON("**结论**\n\n<at id=ou_zhang></at> <at id=ou_b></at> 请确认")
	var card struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &card); err != nil || len(card.Body.Elements) == 0 {
		t.Fatalf("bad card json: %v %s", err, body)
	}
	md := card.Body.Elements[0].Content
	if !strings.Contains(md, `<at id=ou_zhang></at>`) || !strings.Contains(md, `<at id=ou_b></at>`) {
		t.Fatalf("card lost at tags: %q", md)
	}
}

func TestParseCollabMessagePage(t *testing.T) {
	raw := `{"code":0,"data":{"has_more":true,"page_token":"pt","items":[
		{"message_id":"om_1","msg_type":"text","create_time":"1792759236452","deleted":false,
		 "sender":{"id":"ou_li","id_type":"open_id","sender_type":"user"},
		 "body":{"content":"{\"text\":\"hi\"}"},
		 "mentions":[{"key":"@_user_1","id":"ou_a","id_type":"open_id","name":"Claude-A"}]}]}}`
	msgs, hasMore, token, err := parseCollabMessagePage([]byte(raw))
	if err != nil || !hasMore || token != "pt" || len(msgs) != 1 {
		t.Fatalf("parse: msgs=%v hasMore=%v token=%q err=%v", msgs, hasMore, token, err)
	}
	m := msgs[0]
	if m.ID != "om_1" || m.CreateMs != 1792759236452 || m.SenderType != "user" || m.SenderID != "ou_li" || m.Mentions[0].Name != "Claude-A" {
		t.Fatalf("unexpected message: %+v", m)
	}
	if _, _, _, err := parseCollabMessagePage([]byte(`{"code":230027,"msg":"Lack of necessary permissions"}`)); err == nil {
		t.Fatalf("API error should be returned")
	}
}

func TestCollabIsCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"/list":                  true,
		"  /fork 2":              true,
		"/role 后端负责人":            true,
		"!ls -la":                true,
		"/api/v1/users 这个接口要不要改": false,
		"看下 /list":               false,
		"":                       false,
	} {
		if got := collabIsCommand(text); got != want {
			t.Errorf("collabIsCommand(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCollabOwnerOnlyCommandsAndApprovals(t *testing.T) {
	got := make(chan *core.Message, 4)
	p := &Platform{
		platformName: "feishu",
		collab:       newCollabRuntime(map[string]any{"collab": true, "collab_owner": "ou_owner"}),
		handler:      func(_ core.Platform, msg *core.Message) { got <- msg },
	}
	dispatch := func(user, text string) *core.Message {
		// A non-Feishu reply context makes the owner-only notice fail fast.
		p.dispatchCoreMessage(&core.Message{UserID: user, Content: text, ReplyCtx: "test"})
		select {
		case msg := <-got:
			return msg
		default:
			return nil
		}
	}

	if msg := dispatch("ou_owner", "/list"); msg == nil || msg.CannotApprove {
		t.Fatalf("the owner's command should reach the engine with approval rights: %+v", msg)
	}
	for _, text := range []string{"/fork 1", "!rm -rf /tmp/x"} {
		if msg := dispatch("ou_other", text); msg != nil {
			t.Fatalf("non-owner command %q reached the engine", text)
		}
	}
	if msg := dispatch("ou_other", "这个方案有个问题"); msg == nil || !msg.CannotApprove {
		t.Fatalf("non-owner chat should reach the agent without approval rights: %+v", msg)
	}
}

func TestOnCardActionCollabOwnerOnly(t *testing.T) {
	platformAny, err := New(map[string]any{
		"app_id":             "cli_xxx",
		"app_secret":         "secret",
		"allow_from":         "*",
		"enable_feishu_card": true,
		"collab":             true,
		"collab_owner":       "ou_owner",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ip := platformAny.(*interactivePlatform)
	msgCh := make(chan *core.Message, 4)
	ip.handler = func(_ core.Platform, msg *core.Message) { msgCh <- msg }

	for _, action := range []string{"perm:allow", "askq:cTE:0:1", "cmd:/list"} {
		if _, err := ip.onCardAction(cardActionEvent("ou_other", action)); err != nil {
			t.Fatalf("onCardAction(%s) error = %v", action, err)
		}
		waitNoMessage(t, msgCh, 100*time.Millisecond, "non-owner "+action)
	}
	if _, err := ip.onCardAction(cardActionEvent("ou_owner", "perm:allow")); err != nil {
		t.Fatalf("onCardAction() error = %v", err)
	}
	if msg := waitAnyMessage(t, msgCh, 2*time.Second, "owner perm:allow"); msg.Content != "allow" || msg.CannotApprove {
		t.Fatalf("owner approval = %+v", msg)
	}
}

// 不发 /role 时仍可从群成员目录得到对方名字和真正的 @ 地址。
func TestCollabDirectoryDiscoveryWithoutRole(t *testing.T) {
	p := &Platform{collab: newCollabRuntime(map[string]any{"collab": true})}
	p.collab.members.Store("chat", &collabMembersEntry{bots: []collabDirectoryBot{{AppID: "app-peer", OpenID: "ou_peer", Name: "Peer"}}})
	st := &collabChatState{Roster: map[string]*collabRosterEntry{}, MentionNames: map[string]string{}}
	p.collabDiscoverBots("chat", st)
	peer := st.Roster["app-peer"]
	if peer == nil || peer.OpenID != "ou_peer" || peer.Role != "" {
		t.Fatalf("missing unregistered bot: %+v", peer)
	}
	text := collabRenderReply("@Peer 请复核", map[string]collabMentionTarget{"Peer": {openID: peer.OpenID, isBot: true}}, 0, 6, "")
	if !strings.Contains(text, "<at id=ou_peer>") {
		t.Fatalf("not a real mention: %s", text)
	}
}
