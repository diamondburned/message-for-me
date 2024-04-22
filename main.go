package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/diamondburned/arikawa/v3/discord"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/ningen/v3"
	"golang.org/x/sync/errgroup"
	"libdb.so/persist"
	persistbadgerdb "libdb.so/persist/driver/badgerdb"
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Environment Variables:\n")
		fmt.Fprintf(os.Stderr, "  $DISCORD_TOKEN    the bot token\n")
		fmt.Fprintf(os.Stderr, "  $STATE_DIRECTORY  the directory to store the bot state\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Documentation:\n")
		fmt.Fprintf(os.Stderr, "  https://libdb.so/message-for-me\n")
	}
	flag.Parse()
}

var (
	stateDirectory string
)

func main() {
	if env := os.Getenv("STATE_DIRECTORY"); env != "" {
		stateDirectory = env
	} else {
		userConfigDir, err := os.UserConfigDir()
		if err != nil {
			slog.Warn(
				"Bot could not get the user's config directory. It will use the current directory instead.",
				"err", err)
			userConfigDir = "."
		}
		stateDirectory = filepath.Join(userConfigDir, "message-for-me")
	}

	slog.Info(
		"This bot will be using a state directory.",
		"state_directory", stateDirectory)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	os.Exit(run(ctx))
}

type botState struct {
	botSettings
	SelfID            discord.UserID
	TargetGuildID     discord.GuildID
	LastAnnouncedTime time.Time
}

var errMalfunction = errors.New("bot is malfunctioning")

func run(ctx context.Context) int {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		slog.Error("This bot requires $DISCORD_TOKEN to be set.")
		return 1
	}

	errg, ctx := errgroup.WithContext(ctx)
	defer errg.Wait()

	// Keep track of the last message that was sent by a person.
	lastSentAuthors, err := persist.NewMap[discord.UserID, discord.MessageID](
		persistbadgerdb.Open,
		filepath.Join(stateDirectory, "last-sent-authors-v1"),
	)
	if err != nil {
		slog.Error(
			"Bot could not open the last-sent-authors database. It will not be able to function.",
			"err", err)
		return 1
	}

	gatewayID := gateway.DefaultIdentifier(token)
	gatewayID.Capabilities = 253 // magic constant from reverse-engineering
	gatewayID.Properties = gateway.IdentifyProperties{
		OS:      runtime.GOOS,
		Browser: "Arikawa",
		Device:  "message-for-me",
	}
	gatewayID.Presence = &gateway.UpdatePresenceCommand{
		// Mark that the bot is perpetually AFK so that it doesn't block any
		// notifications from arriving.
		Status: discord.IdleStatus,
		AFK:    true,
	}

	session := ningen.
		NewWithIdentifier(gatewayID).
		WithContext(ctx)

	var (
		msgCh               = make(chan *gateway.MessageCreateEvent)
		readyCh             = make(chan *gateway.ReadyEvent)
		guildCh             = make(chan *gateway.GuildCreateEvent)
		readySupplementalCh = make(chan *gateway.ReadySupplementalEvent)
	)

	errg.Go(func() error {
		bot := botState{botSettings: settings}
		trySubscribe := func() bool {
			if bot.TargetGuildID.IsValid() {
				return true
			}

			ch, err := session.Cabinet.Channel(settings.TargetChannelID)
			if err != nil {
				slog.Info(
					"The bot tried to get the target channel, but it failed.",
					"err", err)
				return false
			}

			bot.TargetGuildID = ch.GuildID

			session.MemberState.Subscribe(ch.GuildID)
			session.AddSyncHandler(msgCh)

			slog.Info(
				"Bot has subscribed to the target channel's guild. It is now ready to serve.",
				"guild_id", ch.GuildID,
				"channel_id", bot.TargetChannelID)

			return true
		}

		startupTimeout := time.After(10 * time.Second)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()

			case <-startupTimeout:
				return fmt.Errorf("bot has failed to start up in time")

			case ev := <-readyCh:
				bot.SelfID = ev.User.ID

				slog.Info(
					"This bot is online. It is preparing to serve.",
					"bot_id", ev.User.ID,
					"bot_name", ev.User.Tag())

				// When the bot comes online, immediately start subscribing to
				// the guild that it cares about. This tells Discord to start
				// sending us message events for that guild.
				trySubscribe()

			case <-readySupplementalCh:
				trySubscribe()

			case <-guildCh:
				trySubscribe()

			case ev := <-msgCh:
				command, err := parseCommand(session, bot, ev)
				if err != nil {
					slog.Debug(
						"Bot was unable to parse the command due to an internal error.",
						"channel_id", ev.ChannelID,
						"err", err)
					continue
				}
				if command == nil {
					continue
				}

				slog.Info(
					"This bot has received a valid command.",
					"author.id", ev.Author.ID,
					"author.tag", ev.Author.Tag(),
					"command", command.Command,
					"body", command.Body)

				switch command.Command {
				case "announce":
					// For announcing a new message, ensure that the global rate
					// limit is respected.
					if time.Since(bot.LastAnnouncedTime) < bot.MinAnnounceTimeGap {
						sendReply(session, ev, "please wait before sending another announcement.")
						continue
					}

					target, err := session.SendMessage(bot.TargetChannelID, command.Body)
					if err != nil {
						slog.Error(
							"Bot has failed to send the announcement message.",
							"channel_id", bot.TargetChannelID,
							"err", err)

						replyInternalError(session, ev)
						continue
					}

					// Update the last announcement time.
					bot.LastAnnouncedTime = time.Now()

					// Send a reply to the author.
					sendReply(session, ev, "the announcement has been sent.")

					// Store the last message sent by the author.
					if err := lastSentAuthors.Store(ev.Author.ID, target.ID); err != nil {
						slog.Warn(
							"Bot has failed to store the last message sent by the author.",
							"author_id", ev.Author.ID,
							"err", err)
					}

				case "edit":
					// Look up the last message sent by the author.
					lastSent, ok, err := lastSentAuthors.Load(ev.Author.ID)
					if err != nil {
						slog.Error(
							"Bots has failed to look up the last message sent by the author.",
							"author_id", ev.Author.ID,
							"err", err)

						replyInternalError(session, ev)
						continue
					}

					if !ok {
						sendReply(session, ev, "this bot could not find the last announcement you sent.")
						continue
					}

					if _, err := session.EditMessage(bot.TargetChannelID, lastSent, command.Body); err != nil {
						slog.Error(
							"Bot has failed to edit the last announcement message.",
							"channel_id", bot.TargetChannelID,
							"message_id", lastSent,
							"err", err)

						replyInternalError(session, ev)
						continue
					}
				}
			}
		}
	})

	errg.Go(func() error {
		slog.Debug("Bot is now connecting to Discord.")
		return session.Connect(ctx)
	})

	if err := errg.Wait(); err != nil {
		// Try to extract the cause of the cancellation, if any.
		if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
			err = cause
		}

		slog.Error(
			"Bot has been stopped.",
			"err", err)
		return 1
	}

	return 0
}

func replyInternalError(session *ningen.State, msg *gateway.MessageCreateEvent) {
	sendReply(session, msg, "this bot has encountered an internal error. This error has been logged.")
}

func sendReply(session *ningen.State, msg *gateway.MessageCreateEvent, content string) {
	content = msg.Author.Mention() + ", " + content

	_, err := session.SendMessageReply(msg.ChannelID, content, msg.ID)
	if err != nil {
		slog.Error(
			"Bot has failed to deliver a reply.",
			"channel_id", msg.ChannelID,
			"author_id", msg.Author.ID,
			"err", err)
	}
}

// parsedCommand describes a parsed command from a message.
// The bot expects a message of the following format:
//
//	<@botID> command
//	body
//
// The command is case-insensitive.
// The new line is necessary.
type parsedCommand struct {
	Command string
	Body    string
}

// parseCommand parses the command from the message.
// It also performs necessary permission checks.
//
// If the command is invalid or the user doesn't have the permission to use it,
// (nil, nil) is returned. If any of the steps needed to perform those checks
// fail, an error is returned instead.
func parseCommand(dsession *ningen.State, bot botState, msg *gateway.MessageCreateEvent) (*parsedCommand, error) {
	// Ensure we don't invoke any API calls.
	// We shouldn't need to.
	dsession = dsession.Offline()

	// The message must come from the same guild.
	if msg.Member == nil || msg.GuildID != bot.TargetGuildID {
		return nil, nil
	}

	// The message must explicitly mention it.
	if dsession.MessageMentions(&msg.Message)&ningen.MessageMentions == 0 {
		return nil, nil
	}

	// The message must come from a user with the right role.
	if !slices.ContainsFunc(msg.Member.RoleIDs, func(id discord.RoleID) bool {
		return slices.Contains(bot.AllowedRoleIDs, id)
	}) {
		return nil, nil
	}

	// The message must conform to the expected format.

	// It expects a message with at least two lines, the first one being the
	// header and the rest being the body.
	header, body, ok := strings.Cut(msg.Content, "\n")
	if !ok {
		return nil, nil
	}

	// The header must begin with its mention.
	if !strings.HasPrefix(header, bot.SelfID.Mention()) {
		return nil, nil
	}

	// Parse the command out.
	command := header
	command = strings.TrimPrefix(command, bot.SelfID.Mention())
	command = strings.TrimSpace(command)
	command = strings.ToLower(command)

	// The command must be non-empty.
	if command == "" {
		return nil, nil
	}

	// The body must be non-empty.
	if body == "" {
		return nil, nil
	}

	// We now have a valid command.
	return &parsedCommand{
		Command: command,
		Body:    body,
	}, nil
}
