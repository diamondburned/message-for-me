package main

import (
	"time"

	"github.com/diamondburned/arikawa/v3/discord"
)

// botSettings holds the settings for the bot.
type botSettings struct {
	// TargetChannelID is the channel ID of the channel to send the messages to.
	TargetChannelID discord.ChannelID
	// AllowedRoleIDs is a list of role IDs that are allowed to use this bot.
	AllowedRoleIDs []discord.RoleID
	// MinAnnounceTimeGap is the minimum time gap between each announcement.
	MinAnnounceTimeGap time.Duration
}

var settings = botSettings{
	TargetChannelID: 710342070342254613, // #announcements

	AllowedRoleIDs: []discord.RoleID{
		808121046028779602, // @Dev Board
	},

	MinAnnounceTimeGap: 4 * time.Hour,
}
