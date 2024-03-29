// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var cli *whatsmeow.Client
var log waLog.Logger

var logLevel = "WARN"
var debugLogs = flag.Bool("debug", false, "Enable debug logs?")
var dbDialect = flag.String("db-dialect", "sqlite3", "Database dialect (sqlite3 or postgres)")
var dbAddress = flag.String("db-address", "file:whatsmeow.db?_foreign_keys=on", "Database address")
var requestFullSync = flag.Bool("request-full-sync", false, "Request full (1 year) history sync when logging in?")
var deviceName = flag.String("device-name", "whatsmeow", "Name of device shown inside WhatsApp")
var bindSocket = flag.String("bind-socket", "", "Start as HTTP server, and bind to tcp or a unix domain socket")
var pairRejectChan = make(chan bool, 1)

func main() {
	waBinary.IndentXML = true
	flag.Parse()

	if *debugLogs {
		logLevel = "DEBUG"
	}
	if *requestFullSync {
		store.DeviceProps.RequireFullSync = proto.Bool(true)
		store.DeviceProps.HistorySyncConfig = &waProto.DeviceProps_HistorySyncConfig{
			FullSyncDaysLimit:   proto.Uint32(3650),
			FullSyncSizeMbLimit: proto.Uint32(102400),
			StorageQuotaMb:      proto.Uint32(102400),
		}
	}
	store.DeviceProps.Os = proto.String(*deviceName)
	log = waLog.Stdout("Main", logLevel, true)

	dbLog := waLog.Stdout("Database", logLevel, true)
	storeContainer, err := sqlstore.New(*dbDialect, *dbAddress, dbLog)
	if err != nil {
		log.Errorf("Failed to connect to database: %v", err)
		return
	}
	device, err := storeContainer.GetFirstDevice()
	if err != nil {
		log.Errorf("Failed to get device: %v", err)
		return
	}

	cli = whatsmeow.NewClient(device, waLog.Stdout("Client", logLevel, true))
	var isWaitingForPair atomic.Bool
	cli.PrePairCallback = func(jid types.JID, platform, businessName string) bool {
		isWaitingForPair.Store(true)
		defer isWaitingForPair.Store(false)
		log.Infof("Pairing %s (platform: %q, business name: %q). Type r within 3 seconds to reject pair", jid, platform, businessName)
		select {
		case reject := <-pairRejectChan:
			if reject {
				log.Infof("Rejecting pair")
				return false
			}
		case <-time.After(3 * time.Second):
		}
		log.Infof("Accepting pair")
		return true
	}

	cli.AddEventHandler(handler)
	err = cli.Connect()
	if err != nil {
		log.Errorf("Failed to connect: %v", err)
		return
	}

	c := make(chan os.Signal)
	input := make(chan string)
	output := make(chan string)
	server := makeServer(*bindSocket)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer close(input)
		scan := bufio.NewScanner(os.Stdin)
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if len(line) > 0 {
				input <- line
			}
		}
	}()
	if *bindSocket != "" {
		go func() {
			log.Infof("Starting server on %v", *bindSocket)
			var listener net.Listener
			if strings.HasPrefix(*bindSocket, "/") {
				listener, err = net.Listen("unix", *bindSocket)
				if err != nil {
					log.Errorf("Error creating Unix domain socket listener: %v", err)
				}
				defer os.Remove(*bindSocket) // Clean up the socket file on exit

			} else {
				listener, err = net.Listen("tcp", *bindSocket)
				if err != nil {
					log.Errorf("Error creating tcp listener: %v", err)
				}
			}
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				log.Errorf("HTTP server error: %v", err)
			}
		}()
	}
	for {
		select {
		case <-c:
			log.Infof("Interrupt received, exiting")
			cli.Disconnect()
			if *bindSocket != "" {
				shutdownCtx, shutdownRelease := context.WithTimeout(context.Background(), 10*time.Second)
				defer shutdownRelease()

				if err := server.Shutdown(shutdownCtx); err != nil {
					log.Errorf("HTTP server shutdown error: %v", err)
				}
			}
			return
		case cmd := <-input:
			if len(cmd) == 0 {
				log.Infof("Stdin closed, exiting")
				cli.Disconnect()
				return
			}
			if isWaitingForPair.Load() {
				if cmd == "r" {
					pairRejectChan <- true
				} else if cmd == "a" {
					pairRejectChan <- false
				}
				continue
			}
			args := strings.Fields(cmd)
			cmd = args[0]
			args = args[1:]
			go handleCmd(strings.ToLower(cmd), args, output)
			out := <-output
			fmt.Print(out)
		}
	}
}

func parseJID(arg string) (types.JID, bool) {
	if arg[0] == '+' {
		arg = arg[1:]
	}
	if !strings.ContainsRune(arg, '@') {
		return types.NewJID(arg, types.DefaultUserServer), true
	} else {
		recipient, err := types.ParseJID(arg)
		if err != nil {
			log.Errorf("Invalid JID %s: %v", arg, err)
			return recipient, false
		} else if recipient.User == "" {
			log.Errorf("Invalid JID %s: no server specified", arg)
			return recipient, false
		}
		return recipient, true
	}
}

func handleCmd(cmd string, args []string, output chan<- string) {
	switch cmd {
	case "pair-phone":
		if len(args) < 1 {
			log.Errorf("Usage: pair-phone <number>")
			goto sendsome
		}
		linkingCode, err := cli.PairPhone(args[0], true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			panic(err)
		}
		output <- linkingCode
	case "list-contacts":
		contacts, _ := cli.Store.Contacts.GetAllContacts()
		jsonString, err := json.Marshal(contacts)
		if err != nil {
			output <- "{}"
		}
		output <- string(jsonString)
	case "send-img":
		if len(args) < 2 {
			log.Errorf("Usage: send-img <jid> <image path> [caption]")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			log.Errorf("Failed to read %s: %v", args[0], err)
			goto sendsome
		}
		uploaded, err := cli.Upload(context.Background(), data, whatsmeow.MediaImage)
		if err != nil {
			log.Errorf("Failed to upload file: %v", err)
			goto sendsome
		}
		msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
			Caption:       proto.String(strings.Join(args[2:], " ")),
			Url:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(http.DetectContentType(data)),
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		}}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			log.Errorf("Error sending image message: %v", err)
		} else {
			log.Infof("Image message sent (server timestamp: %s)", resp.Timestamp)
		}
	case "send-file":
		if len(args) < 2 {
			log.Errorf("Usage: send-file <jid> <file path> [caption]")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			log.Errorf("Failed to read %s: %v", args[0], err)
			goto sendsome
		}
		uploaded, err := cli.Upload(context.Background(), data, whatsmeow.MediaDocument)
		if err != nil {
			log.Errorf("Failed to upload file: %v", err)
			goto sendsome
		}
		msg := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			Caption:       proto.String(strings.Join(args[2:], " ")),
			Url:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(http.DetectContentType(data)),
			FileName:      proto.String("file"),
			FileEncSha256: uploaded.FileEncSHA256,
			FileSha256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		}}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			log.Errorf("Error sending file message: %v", err)
		} else {
			log.Infof("File message sent (server timestamp: %s)", resp.Timestamp)
		}
	case "send":
		if len(args) < 2 {
			log.Errorf("Usage: send <jid> <text>")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		msg := &waProto.Message{Conversation: proto.String(strings.Join(args[1:], " "))}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			log.Errorf("Error sending message: %v", err)
		} else {
			log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
		}
	case "reconnect":
		cli.Disconnect()
		qrChan, _ := cli.GetQRChannel(context.Background())
		err := cli.Connect()
		if err != nil {
			log.Errorf("Failed to connect: %v", err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				// Render the QR code here
				// e.g. qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				// or just manually `echo 2@... | qrencode -t ansiutf8` in a terminal
				output <- evt.Code
			} else {
				log.Infof("Login event: %v", evt.Event)
			}
		}
	case "logout":
		err := cli.Logout()
		if err != nil {
			log.Errorf("Error logging out: %v", err)
		} else {
			log.Infof("Successfully logged out")
		}
	case "appstate":
		if len(args) < 1 {
			log.Errorf("Usage: appstate <types...>")
			goto sendsome
		}
		names := []appstate.WAPatchName{appstate.WAPatchName(args[0])}
		if args[0] == "all" {
			names = []appstate.WAPatchName{appstate.WAPatchRegular, appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow, appstate.WAPatchCriticalUnblockLow, appstate.WAPatchCriticalBlock}
		}
		resync := len(args) > 1 && args[1] == "resync"
		for _, name := range names {
			err := cli.FetchAppState(name, resync, false)
			if err != nil {
				log.Errorf("Failed to sync app state: %v", err)
			}
		}
	case "request-appstate-key":
		if len(args) < 1 {
			log.Errorf("Usage: request-appstate-key <ids...>")
			goto sendsome
		}
		var keyIDs = make([][]byte, len(args))
		for i, id := range args {
			decoded, err := hex.DecodeString(id)
			if err != nil {
				log.Errorf("Failed to decode %s as hex: %v", id, err)
				goto sendsome
			}
			keyIDs[i] = decoded
		}
		cli.DangerousInternals().RequestAppStateKeys(context.Background(), keyIDs)

	case "whoami":
		output <- fmt.Sprint(cli.DangerousInternals().GetOwnID())

	case "unavailable-request":
		if len(args) < 3 {
			log.Errorf("Usage: unavailable-request <chat JID> <sender JID> <message ID>")
			goto sendsome
		}
		chat, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		sender, ok := parseJID(args[1])
		if !ok {
			goto sendsome
		}
		resp, err := cli.SendMessage(
			context.Background(),
			cli.Store.ID.ToNonAD(),
			cli.BuildUnavailableMessageRequest(chat, sender, args[2]),
			whatsmeow.SendRequestExtra{Peer: true},
		)
		output <- fmt.Sprintln(resp)
		output <- fmt.Sprintln(err)
	case "checkuser":
		if len(args) < 1 {
			log.Errorf("Usage: checkuser <phone numbers...>")
			goto sendsome
		}
		resp, err := cli.IsOnWhatsApp(args)
		if err != nil {
			log.Errorf("Failed to check if users are on WhatsApp:", err)
		} else {
			for _, item := range resp {
				if item.VerifiedName != nil {
					log.Infof("%s: on whatsapp: %t, JID: %s, business name: %s", item.Query, item.IsIn, item.JID, item.VerifiedName.Details.GetVerifiedName())
				} else {
					log.Infof("%s: on whatsapp: %t, JID: %s", item.Query, item.IsIn, item.JID)
				}
			}
		}
	case "checkupdate":
		resp, err := cli.CheckUpdate()
		if err != nil {
			log.Errorf("Failed to check for updates: %v", err)
		} else {
			log.Debugf("Version data: %#v", resp)
			if resp.ParsedVersion == store.GetWAVersion() {
				log.Infof("Client is up to date")
			} else if store.GetWAVersion().LessThan(resp.ParsedVersion) {
				log.Warnf("Client is outdated")
			} else {
				log.Infof("Client is newer than latest")
			}
		}
	case "subscribepresence":
		if len(args) < 1 {
			log.Errorf("Usage: subscribepresence <jid>")
			goto sendsome
		}
		jid, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		err := cli.SubscribePresence(jid)
		if err != nil {
			output <- fmt.Sprintln(err)
		}
	case "presence":
		if len(args) == 0 {
			log.Errorf("Usage: presence <available/unavailable>")
			goto sendsome
		}
		output <- fmt.Sprint(cli.SendPresence(types.Presence(args[0])))
	case "chatpresence":
		if len(args) == 2 {
			args = append(args, "")
		} else if len(args) < 2 {
			log.Errorf("Usage: chatpresence <jid> <composing/paused> [audio]")
			goto sendsome
		}
		jid, _ := types.ParseJID(args[0])
		output <- fmt.Sprint(cli.SendChatPresence(jid, types.ChatPresence(args[1]), types.ChatPresenceMedia(args[2])))
	case "privacysettings":
		resp, err := cli.TryFetchPrivacySettings(false)
		if err != nil {
			output <- fmt.Sprintln(err)
		} else {
			output <- fmt.Sprintf("%+v\n", resp)
		}
	case "getuser":
		if len(args) < 1 {
			log.Errorf("Usage: getuser <jids...>")
			goto sendsome
		}
		var jids []types.JID
		for _, arg := range args {
			jid, ok := parseJID(arg)
			if !ok {
				goto sendsome
			}
			jids = append(jids, jid)
		}
		resp, err := cli.GetUserInfo(jids)
		if err != nil {
			log.Errorf("Failed to get user info: %v", err)
		} else {
			for jid, info := range resp {
				log.Infof("%s: %+v", jid, info)
			}
		}
	case "mediaconn":
		conn, err := cli.DangerousInternals().RefreshMediaConn(false)
		if err != nil {
			log.Errorf("Failed to get media connection: %v", err)
		} else {
			log.Infof("Media connection: %+v", conn)
		}
	case "getavatar":
		if len(args) < 1 {
			log.Errorf("Usage: getavatar <jid> [existing ID] [--preview] [--community]")
			goto sendsome
		}
		jid, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		existingID := ""
		if len(args) > 2 {
			existingID = args[2]
		}
		var preview, isCommunity bool
		for _, arg := range args {
			if arg == "--preview" {
				preview = true
			} else if arg == "--community" {
				isCommunity = true
			}
		}
		pic, err := cli.GetProfilePictureInfo(jid, &whatsmeow.GetProfilePictureParams{
			Preview:     preview,
			IsCommunity: isCommunity,
			ExistingID:  existingID,
		})
		if err != nil {
			log.Errorf("Failed to get avatar: %v", err)
		} else if pic != nil {
			log.Infof("Got avatar ID %s: %s", pic.ID, pic.URL)
		} else {
			log.Infof("No avatar found")
		}
	case "getgroup":
		if len(args) < 1 {
			log.Errorf("Usage: getgroup <jid>")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetGroupInfo(group)
		if err != nil {
			log.Errorf("Failed to get group info: %v", err)
		} else {
			log.Infof("Group info: %+v", resp)
		}
	case "subgroups":
		if len(args) < 1 {
			log.Errorf("Usage: subgroups <jid>")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetSubGroups(group)
		if err != nil {
			log.Errorf("Failed to get subgroups: %v", err)
		} else {
			for _, sub := range resp {
				log.Infof("Subgroup: %+v", sub)
			}
		}
	case "communityparticipants":
		if len(args) < 1 {
			log.Errorf("Usage: communityparticipants <jid>")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetLinkedGroupsParticipants(group)
		if err != nil {
			log.Errorf("Failed to get community participants: %v", err)
		} else {
			log.Infof("Community participants: %+v", resp)
		}
	case "listgroups":
		groups, err := cli.GetJoinedGroups()
		if err != nil {
			log.Errorf("Failed to get group list: %v", err)
		} else {
			for _, group := range groups {
				log.Infof("%+v", group)
			}
		}
	case "getinvitelink":
		if len(args) < 1 {
			log.Errorf("Usage: getinvitelink <jid> [--reset]")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		} else if group.Server != types.GroupServer {
			log.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetGroupInviteLink(group, len(args) > 1 && args[1] == "--reset")
		if err != nil {
			log.Errorf("Failed to get group invite link: %v", err)
		} else {
			log.Infof("Group invite link: %s", resp)
		}
	case "queryinvitelink":
		if len(args) < 1 {
			log.Errorf("Usage: queryinvitelink <link>")
			goto sendsome
		}
		resp, err := cli.GetGroupInfoFromLink(args[0])
		if err != nil {
			log.Errorf("Failed to resolve group invite link: %v", err)
		} else {
			log.Infof("Group info: %+v", resp)
		}
	case "querybusinesslink":
		if len(args) < 1 {
			log.Errorf("Usage: querybusinesslink <link>")
			goto sendsome
		}
		resp, err := cli.ResolveBusinessMessageLink(args[0])
		if err != nil {
			log.Errorf("Failed to resolve business message link: %v", err)
		} else {
			log.Infof("Business info: %+v", resp)
		}
	case "joininvitelink":
		if len(args) < 1 {
			log.Errorf("Usage: acceptinvitelink <link>")
			goto sendsome
		}
		groupID, err := cli.JoinGroupWithLink(args[0])
		if err != nil {
			log.Errorf("Failed to join group via invite link: %v", err)
		} else {
			log.Infof("Joined %s", groupID)
		}
	case "getstatusprivacy":
		resp, err := cli.GetStatusPrivacy()
		output <- fmt.Sprintln(err)
		output <- fmt.Sprintln(resp)
	case "setdisappeartimer":
		if len(args) < 2 {
			log.Errorf("Usage: setdisappeartimer <jid> <days>")
			goto sendsome
		}
		days, err := strconv.Atoi(args[1])
		if err != nil {
			log.Errorf("Invalid duration: %v", err)
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		err = cli.SetDisappearingTimer(recipient, time.Duration(days)*24*time.Hour)
		if err != nil {
			log.Errorf("Failed to set disappearing timer: %v", err)
		}
	case "sendpoll":
		if len(args) < 7 {
			log.Errorf("Usage: sendpoll <jid> <max answers> <question> -- <option 1> / <option 2> / ...")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		maxAnswers, err := strconv.Atoi(args[1])
		if err != nil {
			log.Errorf("Number of max answers must be an integer")
			goto sendsome
		}
		remainingArgs := strings.Join(args[2:], " ")
		question, optionsStr, _ := strings.Cut(remainingArgs, "--")
		question = strings.TrimSpace(question)
		options := strings.Split(optionsStr, "/")
		for i, opt := range options {
			options[i] = strings.TrimSpace(opt)
		}
		resp, err := cli.SendMessage(context.Background(), recipient, cli.BuildPollCreation(question, options, maxAnswers))
		if err != nil {
			log.Errorf("Error sending message: %v", err)
		} else {
			log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
		}
	case "multisend":
		if len(args) < 3 {
			log.Errorf("Usage: multisend <jids...> -- <text>")
			goto sendsome
		}
		var recipients []types.JID
		for len(args) > 0 && args[0] != "--" {
			recipient, ok := parseJID(args[0])
			args = args[1:]
			if !ok {
				goto sendsome
			}
			recipients = append(recipients, recipient)
		}
		if len(args) == 0 {
			log.Errorf("Usage: multisend <jids...> -- <text> (the -- is required)")
			goto sendsome
		}
		msg := &waProto.Message{Conversation: proto.String(strings.Join(args[1:], " "))}
		for _, recipient := range recipients {
			go func(recipient types.JID) {
				resp, err := cli.SendMessage(context.Background(), recipient, msg)
				if err != nil {
					log.Errorf("Error sending message to %s: %v", recipient, err)
				} else {
					log.Infof("Message sent to %s (server timestamp: %s)", recipient, resp.Timestamp)
				}
			}(recipient)
		}
	case "react":
		if len(args) < 3 {
			log.Errorf("Usage: react <jid> <message ID> <reaction>")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		messageID := args[1]
		fromMe := false
		if strings.HasPrefix(messageID, "me:") {
			fromMe = true
			messageID = messageID[len("me:"):]
		}
		reaction := args[2]
		if reaction == "remove" {
			reaction = ""
		}
		msg := &waProto.Message{
			ReactionMessage: &waProto.ReactionMessage{
				Key: &waProto.MessageKey{
					RemoteJid: proto.String(recipient.String()),
					FromMe:    proto.Bool(fromMe),
					Id:        proto.String(messageID),
				},
				Text:              proto.String(reaction),
				SenderTimestampMs: proto.Int64(time.Now().UnixMilli()),
			},
		}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			log.Errorf("Error sending reaction: %v", err)
		} else {
			log.Infof("Reaction sent (server timestamp: %s)", resp.Timestamp)
		}
	case "revoke":
		if len(args) < 2 {
			log.Errorf("Usage: revoke <jid> <message ID>")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		messageID := args[1]
		resp, err := cli.SendMessage(context.Background(), recipient, cli.BuildRevoke(recipient, types.EmptyJID, messageID))
		if err != nil {
			log.Errorf("Error sending revocation: %v", err)
		} else {
			log.Infof("Revocation sent (server timestamp: %s)", resp.Timestamp)
		}
	case "setstatus":
		if len(args) == 0 {
			log.Errorf("Usage: setstatus <message>")
			goto sendsome
		}
		err := cli.SetStatusMessage(strings.Join(args, " "))
		if err != nil {
			log.Errorf("Error setting status message: %v", err)
		} else {
			log.Infof("Status updated")
		}
	case "archive":
		if len(args) < 2 {
			log.Errorf("Usage: archive <jid> <action>")
			goto sendsome
		}
		target, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		action, err := strconv.ParseBool(args[1])
		if err != nil {
			log.Errorf("invalid second argument: %v", err)
			goto sendsome
		}

		err = cli.SendAppState(appstate.BuildArchive(target, action, time.Time{}, nil))
		if err != nil {
			log.Errorf("Error changing chat's archive state: %v", err)
		}
	case "mute":
		if len(args) < 2 {
			log.Errorf("Usage: mute <jid> <action>")
			goto sendsome
		}
		target, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		action, err := strconv.ParseBool(args[1])
		if err != nil {
			log.Errorf("invalid second argument: %v", err)
			goto sendsome
		}

		err = cli.SendAppState(appstate.BuildMute(target, action, 1*time.Hour))
		if err != nil {
			log.Errorf("Error changing chat's mute state: %v", err)
		}
	case "pin":
		if len(args) < 2 {
			log.Errorf("Usage: pin <jid> <action>")
			goto sendsome
		}
		target, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		action, err := strconv.ParseBool(args[1])
		if err != nil {
			log.Errorf("invalid second argument: %v", err)
			goto sendsome
		}

		err = cli.SendAppState(appstate.BuildPin(target, action))
		if err != nil {
			log.Errorf("Error changing chat's pin state: %v", err)
		}
	case "getblocklist":
		blocklist, err := cli.GetBlocklist()
		if err != nil {
			log.Errorf("Failed to get blocked contacts list: %v", err)
		} else {
			log.Infof("Blocklist: %+v", blocklist)
		}
	case "block":
		if len(args) < 1 {
			log.Errorf("Usage: block <jid>")
			goto sendsome
		}
		jid, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		resp, err := cli.UpdateBlocklist(jid, events.BlocklistChangeActionBlock)
		if err != nil {
			log.Errorf("Error updating blocklist: %v", err)
		} else {
			log.Infof("Blocklist updated: %+v", resp)
		}
	case "unblock":
		if len(args) < 1 {
			log.Errorf("Usage: unblock <jid>")
			goto sendsome
		}
		jid, ok := parseJID(args[0])
		if !ok {
			goto sendsome
		}
		resp, err := cli.UpdateBlocklist(jid, events.BlocklistChangeActionUnblock)
		if err != nil {
			log.Errorf("Error updating blocklist: %v", err)
		} else {
			log.Infof("Blocklist updated: %+v", resp)
		}
	}
	goto sendsome // the catchall
sendsome:
	output <- ""
}

var historySyncID int32
var startupTime = time.Now().Unix()

func handler(rawEvt interface{}) {
	switch evt := rawEvt.(type) {
	case *events.AppStateSyncComplete:
		if len(cli.Store.PushName) > 0 && evt.Name == appstate.WAPatchCriticalBlock {
			err := cli.SendPresence(types.PresenceAvailable)
			if err != nil {
				log.Warnf("Failed to send available presence: %v", err)
			} else {
				log.Infof("Marked self as available")
			}
		}
	case *events.Connected, *events.PushNameSetting:
		if len(cli.Store.PushName) == 0 {
			return
		}
		// Send presence available when connecting and when the pushname is changed.
		// This makes sure that outgoing messages always have the right pushname.
		err := cli.SendPresence(types.PresenceAvailable)
		if err != nil {
			log.Warnf("Failed to send available presence: %v", err)
		} else {
			log.Infof("Marked self as available")
		}
	case *events.StreamReplaced:
		os.Exit(0)
	case *events.Message:
		metaParts := []string{fmt.Sprintf("pushname: %s", evt.Info.PushName), fmt.Sprintf("timestamp: %s", evt.Info.Timestamp)}
		if evt.Info.Type != "" {
			metaParts = append(metaParts, fmt.Sprintf("type: %s", evt.Info.Type))
		}
		if evt.Info.Category != "" {
			metaParts = append(metaParts, fmt.Sprintf("category: %s", evt.Info.Category))
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "view once")
		}
		if evt.IsViewOnce {
			metaParts = append(metaParts, "ephemeral")
		}
		if evt.IsViewOnceV2 {
			metaParts = append(metaParts, "ephemeral (v2)")
		}
		if evt.IsDocumentWithCaption {
			metaParts = append(metaParts, "document with caption")
		}
		if evt.IsEdit {
			metaParts = append(metaParts, "edit")
		}

		log.Infof("Received message %s from %s (%s): %+v", evt.Info.ID, evt.Info.SourceString(), strings.Join(metaParts, ", "), evt.Message)

		if evt.Message.GetPollUpdateMessage() != nil {
			decrypted, err := cli.DecryptPollVote(evt)
			if err != nil {
				log.Errorf("Failed to decrypt vote: %v", err)
			} else {
				log.Infof("Selected options in decrypted vote:")
				for _, option := range decrypted.SelectedOptions {
					log.Infof("- %X", option)
				}
			}
		} else if evt.Message.GetEncReactionMessage() != nil {
			decrypted, err := cli.DecryptReaction(evt)
			if err != nil {
				log.Errorf("Failed to decrypt encrypted reaction: %v", err)
			} else {
				log.Infof("Decrypted reaction: %+v", decrypted)
			}
		}

		img := evt.Message.GetImageMessage()
		if img != nil {
			data, err := cli.Download(img)
			if err != nil {
				log.Errorf("Failed to download image: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(img.GetMimetype())
			filename := fmt.Sprintf("%s%s", evt.Info.ID, exts[0])

			dirPath := "downloads"
			if _, err := os.Stat(dirPath); errors.Is(err, os.ErrNotExist) {
				err := os.Mkdir(dirPath, os.ModePerm)
				if err != nil {
					log.Errorf("Failed to create downloads directory: %v", err)
					return
				}
			}

			path := path.Join(dirPath, filename)
			err = os.WriteFile(path, data, 0600)
			if err != nil {
				log.Errorf("Failed to save image: %v", err)
				return
			}
			log.Infof("Saved image in message to %s", path)

			jsonData := map[string]string{"type": "image",
				"messageId": evt.Info.ID,
				"contact":   evt.Info.SourceString(),
				"path":      path}
			jsonString, err := json.Marshal(jsonData)
			if err != nil {
				log.Errorf("Failed to generate notification : %v", err)
				return
			}
			fmt.Println(string(jsonString))
		}

		docm := evt.Message.GetDocumentMessage()
		if docm != nil {
			data, err := cli.Download(docm)
			if err != nil {
				log.Errorf("Failed to download file: %v", err)
				return
			}
			exts, _ := mime.ExtensionsByType(docm.GetMimetype())
			filename := evt.Info.ID
			if len(exts) != 0 {
				filename = fmt.Sprintf("%s%s", evt.Info.ID, exts[0])
			}

			dirPath := "downloads"
			if _, err := os.Stat(dirPath); errors.Is(err, os.ErrNotExist) {
				err := os.Mkdir(dirPath, os.ModePerm)
				if err != nil {
					log.Errorf("Failed to create downloads directory: %v", err)
					return
				}
			}

			path := path.Join(dirPath, filename)
			err = os.WriteFile(path, data, 0600)
			if err != nil {
				log.Errorf("Failed to save file: %v", err)
				return
			}
			log.Infof("Saved file in message to %s", path)

			jsonData := map[string]string{"type": "file",
				"messageId": evt.Info.ID,
				"contact":   evt.Info.SourceString(),
				"path":      path}
			jsonString, err := json.Marshal(jsonData)
			if err != nil {
				log.Errorf("Failed to generate notification : %v", err)
				return
			}
			fmt.Println(string(jsonString))
		}
	case *events.Receipt:
		if evt.Type == types.ReceiptTypeRead || evt.Type == types.ReceiptTypeReadSelf {
			log.Infof("%v was read by %s at %s", evt.MessageIDs, evt.SourceString(), evt.Timestamp)
		} else if evt.Type == types.ReceiptTypeDelivered {
			log.Infof("%s was delivered to %s at %s", evt.MessageIDs[0], evt.SourceString(), evt.Timestamp)
		}
	case *events.Presence:
		if evt.Unavailable {
			if evt.LastSeen.IsZero() {
				log.Infof("%s is now offline", evt.From)
			} else {
				log.Infof("%s is now offline (last seen: %s)", evt.From, evt.LastSeen)
			}
		} else {
			log.Infof("%s is now online", evt.From)
		}
	case *events.HistorySync:
		return
		/*
			id := atomic.AddInt32(&historySyncID, 1)
			fileName := fmt.Sprintf("history-%d-%d.json", startupTime, id)
			file, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE, 0600)
			if err != nil {
				log.Errorf("Failed to open file to write history sync: %v", err)
				return
			}
			enc := json.NewEncoder(file)
			enc.SetIndent("", "  ")
			err = enc.Encode(evt.Data)
			if err != nil {
				log.Errorf("Failed to write history sync: %v", err)
				return
			}
			log.Infof("Wrote history sync to %s", fileName)
			_ = file.Close()
		*/
	case *events.AppState:
		log.Debugf("App state event: %+v / %+v", evt.Index, evt.SyncActionValue)
	case *events.KeepAliveTimeout:
		log.Debugf("Keepalive timeout event: %+v", evt)
	case *events.KeepAliveRestored:
		log.Debugf("Keepalive restored")
	case *events.Blocklist:
		log.Infof("Blocklist event: %+v", evt)
	}
}
