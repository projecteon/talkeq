package discord

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/xackery/talkeq/config"
	"github.com/xackery/talkeq/request"
	"github.com/xackery/talkeq/tlog"
)

const (
	//ActionMessage means discord sent the message
	ActionMessage = "message"
)

// Discord represents a discord connection
type Discord struct {
	ctx           context.Context
	cancel        context.CancelFunc
	isConnected   bool
	mu            sync.RWMutex
	config        config.Discord
	conn          *discordgo.Session
	subscribers   []func(interface{}) error
	id            string
	lastMessageID string
	lastChannelID string
	commands      map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate) (string, error)
}

// New creates a new discord connect
func New(ctx context.Context, config config.Discord) (*Discord, error) {
	ctx, cancel := context.WithCancel(ctx)

	t := &Discord{
		ctx:    ctx,
		cancel: cancel,
		config: config,
	}
	t.commands = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate) (string, error){
		"who": t.who,
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	tlog.Debugf("[discord] verifying configuration")

	if !config.IsEnabled {
		return t, nil
	}

	if config.ClientID == "" {
		return nil, fmt.Errorf("client_id must be set. Visit https://github.com/xackery/talkeq to learn more")
	}

	if config.Token == "" {
		return nil, fmt.Errorf("bot_token must be set. Visit https://github.com/xackery/talkeq to learn more")
	}

	if config.ServerID == "" {
		return nil, fmt.Errorf("server_id must be set. On discord, right click your server's icon on very left, and Copy ID, and place it in talkeq.conf in the server_id section")
	}

	return t, nil
}

// Connect establishes a new connection with Discord
func (t *Discord) Connect(ctx context.Context) error {
	var err error
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.config.IsEnabled {
		tlog.Debugf("[discord] is disabled, skipping connect")
		return nil
	}

	tlog.Infof("[discord] connecting to server_id %s...", t.config.ServerID)

	if t.conn != nil {
		t.conn.Close()
		t.conn = nil
		t.cancel()
	}
	t.ctx, t.cancel = context.WithCancel(ctx)

	t.conn, err = discordgo.New("Bot " + t.config.Token)
	if err != nil {
		return fmt.Errorf("new: %w", err)
	}

	t.conn.StateEnabled = true
	t.conn.AddHandler(t.handleMessage)
	t.conn.AddHandler(t.handleCommand)

	err = t.conn.Open()
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	go t.loop(ctx)

	t.isConnected = true
	tlog.Infof("[discord] connected successfully")
	var st *discordgo.Channel
	for _, route := range t.config.Routes {
		if !route.IsEnabled {
			continue
		}
		st, err = t.conn.Channel(route.Trigger.ChannelID)
		if err != nil {
			tlog.Errorf("[discord] your bot appears to not be allowed to listen to route %s's channel %s. visit https://discordapp.com/oauth2/authorize?&client_id=%s&scope=bot&permissions=268504080 and authorize", route.Target, route.Trigger.ChannelID, t.config.ClientID)
			if runtime.GOOS == "windows" {
				option := ""
				fmt.Println("press a key then enter to exit.")
				fmt.Scan(&option)
			}
			os.Exit(1)
		}
		tlog.Infof("[discord->%s] registered route for chat in #%s", route.Target, st.Name)
	}

	myUser, err := t.conn.User("@me")
	if err != nil {
		return fmt.Errorf("get my username: %w", err)
	}

	t.id = myUser.ID
	tlog.Debugf("[discord] @me id: %s", t.id)

	err = t.StatusUpdate(ctx, 0, "Status: Online")
	if err != nil {
		return err
	}

	// TODO: Get this working
	if 1 == 0 {
		err = t.whoRegister()
		if err != nil {
			return fmt.Errorf("whoRegister: %w", err)
		}
	}

	return nil
}

func (t *Discord) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			tlog.Debugf("[discord] loop exit")
			return
		default:
		}

		time.Sleep(60 * time.Second)
	}
}

// StatusUpdate updates the status text on discord
func (t *Discord) StatusUpdate(ctx context.Context, online int, customText string) error {
	var err error
	if customText != "" {
		err = t.conn.UpdateGameStatus(0, customText)
		if err != nil {
			return err
		}
		return nil
	}
	tmpl := template.New("online")
	tmpl.Parse(t.config.BotStatus)

	buf := new(bytes.Buffer)
	tmpl.Execute(buf, struct {
		PlayerCount int
	}{
		online,
	})

	err = t.conn.UpdateGameStatus(0, buf.String())
	if err != nil {
		return err
	}
	return nil
}

// IsConnected returns if a connection is established
func (t *Discord) IsConnected() bool {
	t.mu.RLock()
	isConnected := t.isConnected
	t.mu.RUnlock()
	return isConnected
}

// Disconnect stops a previously started connection with Discord.
// If called while a connection is not active, returns nil
func (t *Discord) Disconnect(ctx context.Context) error {
	if !t.config.IsEnabled {
		tlog.Debugf("[discord] is disabled, skipping disconnect")
		return nil
	}
	if !t.isConnected {
		tlog.Debugf("[discord] already disconnected, skipping disconnect")
		return nil
	}
	err := t.conn.Close()
	if err != nil {
		tlog.Warnf("[discord] disconnect failed: %s", err)
	}
	t.conn = nil
	t.isConnected = false
	return nil
}

// Send sends a message to discord
func (t *Discord) Send(req request.DiscordSend) error {
	if !t.config.IsEnabled {
		return fmt.Errorf("not enabled")
	}

	if !t.isConnected {
		return fmt.Errorf("not connected")
	}

	msg, err := t.conn.ChannelMessageSendComplex(req.ChannelID, &discordgo.MessageSend{
		Content:         req.Message,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		return fmt.Errorf("ChannelMessageSend: %w", err)
	}
	t.lastMessageID = msg.ID
	t.lastChannelID = msg.ChannelID
	return nil
}

// Subscribe listens for new events on discord
func (t *Discord) Subscribe(ctx context.Context, onMessage func(interface{}) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subscribers = append(t.subscribers, onMessage)
	return nil
}

func sanitize(data string) string {
	data = strings.Replace(data, `%`, "&PCT;", -1)
	re := regexp.MustCompile("[^\x00-\x7F]+")
	data = re.ReplaceAllString(data, "")
	return data
}

// SetChannelName is used for voice channel setting via SQLReport
func (t *Discord) SetChannelName(channelID string, name string) error {
	if !t.isConnected {
		return fmt.Errorf("discord not connected")
	}

	if _, err := t.conn.ChannelEdit(channelID, &discordgo.ChannelEdit{Name: name}); err != nil {
		return fmt.Errorf("edit channel failed: %w", err)
	}
	tlog.Debugf("[discord] setting channel to %s", name)
	return nil
}

// GetIGNName returns an IGN: tagged name from discord if applicable
func (t *Discord) GetIGNName(s *discordgo.Session, serverID string, userid string) string {
	if serverID == "" {
		serverID = t.config.ServerID
	}
	member, err := s.GuildMember(serverID, userid)
	if err != nil {
		tlog.Warnf("[discord] guildMember failed for author_id %s, server_id %s: %s", userid, serverID, err)
		return ""
	}
	roles, err := s.GuildRoles(serverID)
	if err != nil {
		tlog.Warnf("[discord] guildRoles failed for server_id %s: %s", serverID, err)
		return ""
	}

	for _, role := range member.Roles {
		for _, gRole := range roles {
			if strings.TrimSpace(gRole.ID) != strings.TrimSpace(role) {
				continue
			}
			if !strings.Contains(gRole.Name, "IGN:") {
				continue
			}
			splitStr := strings.Split(gRole.Name, "IGN:")
			if len(splitStr) > 1 {
				return strings.TrimSpace(splitStr[1])
			}
		}
	}
	return ""
}

// LastSentMessage returns the channelID and message ID of last message sent
func (t *Discord) LastSentMessage() (channelID string, messageID string, err error) {
	if !t.config.IsEnabled {
		return "", "", fmt.Errorf("not enabled")
	}
	if !t.isConnected {
		return "", "", fmt.Errorf("not connected")
	}
	return t.lastChannelID, t.lastMessageID, nil
}

// EditMessage lets you edit a previously sent message
func (t *Discord) EditMessage(channelID string, messageID string, message string) error {
	if !t.config.IsEnabled {
		return fmt.Errorf("not enabled")
	}
	if !t.isConnected {
		return fmt.Errorf("not connected")
	}
	msg, err := t.conn.ChannelMessageEdit(channelID, messageID, message)
	if err != nil {
		return fmt.Errorf("edit: %w", err)
	}
	tlog.Debugf("[discord] edited message before: %s, after: %s", messageID, msg.ID)
	return nil
}
