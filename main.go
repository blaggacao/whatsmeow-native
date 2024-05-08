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
	errChan := make(chan error)
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
		    go handleCmd(cmd, args, output, errChan)
			select {
			case err := <-errChan:
			    if err != nil {
			        log.Errorf("%v", err)
			    }
			case out := <-output:
			    fmt.Print(out)
			}
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

func handleCmd(cmd string, args []string, output chan<- string, errChan chan<- error) {
	switch cmd {
	case "pair-phone":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: pair-phone <number>")
			goto sendsome
		}
		linkingCode, err := cli.PairPhone(args[0], true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			errChan <- err
			goto sendsome
		}
		output <- linkingCode
	case "list-contacts":
		contacts, _ := cli.Store.Contacts.GetAllContacts()
		jsonString, err := json.Marshal(contacts)
		if err != nil {
			output <- "{}"
			errChan <- err
			goto sendsome
		}
		output <- string(jsonString)
	case "send-img":
		if len(args) < 2 {
			errChan <- fmt.Errorf("Usage: send-img <jid> <image path> [caption]")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			errChan <- fmt.Errorf("Failed to read %s: %v", args[0], err)
			goto sendsome
		}
		uploaded, err := cli.Upload(context.Background(), data, whatsmeow.MediaImage)
		if err != nil {
			errChan <- fmt.Errorf("Failed to upload file: %v", err)
			goto sendsome
		}
		msg := &waProto.Message{
			ImageMessage: &waProto.ImageMessage{
				Caption:       proto.String(strings.Join(args[2:], " ")),
				Url:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				Mimetype:      proto.String(http.DetectContentType(data)),
				FileEncSha256: uploaded.FileEncSHA256,
				FileSha256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			errChan <- fmt.Errorf("Error sending image message: %v", err)
			goto sendsome
		}
		log.Infof("Image message sent (server timestamp: %s)", resp.Timestamp)
		goto sendsome
	case "send-file":
		if len(args) < 2 {
			errChan <- fmt.Errorf("Usage: send-file <jid> <file path> [caption]")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			errChan <- fmt.Errorf("Failed to read %s: %v", args[0], err)
			goto sendsome
		}
		uploaded, err := cli.Upload(context.Background(), data, whatsmeow.MediaDocument)
		if err != nil {
			errChan <- fmt.Errorf("Failed to upload file: %v", err)
			goto sendsome
		}
		msg := &waProto.Message{
			DocumentMessage: &waProto.DocumentMessage{
				Caption:       proto.String(strings.Join(args[2:], " ")),
				Url:           proto.String(uploaded.URL),
				DirectPath:    proto.String(uploaded.DirectPath),
				MediaKey:      uploaded.MediaKey,
				Mimetype:      proto.String(http.DetectContentType(data)),
				FileName:      proto.String("file"),
				FileEncSha256: uploaded.FileEncSHA256,
				FileSha256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			errChan <- fmt.Errorf("Error sending file message: %v", err)
			goto sendsome
		}
		log.Infof("File message sent (server timestamp: %s)", resp.Timestamp)
		goto sendsome
	case "send":
		if len(args) < 2 {
			errChan <- fmt.Errorf("Usage: send <jid> <text>")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		}
		msg := &waProto.Message{
			Conversation: proto.String(strings.Join(args[1:], " ")),
		}
		resp, err := cli.SendMessage(context.Background(), recipient, msg)
		if err != nil {
			errChan <- fmt.Errorf("Error sending message: %v", err)
			goto sendsome
		}
		log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
		goto sendsome
	case "reconnect":
		cli.Disconnect()
		qrChan, _ := cli.GetQRChannel(context.Background())
		err := cli.Connect()
		if err != nil {
			errChan <- fmt.Errorf("Failed to connect: %v", err)
			goto sendsome
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				output <- evt.Code
			} else {
				log.Infof("Login event: %v", evt.Event)
			}
		}
		goto sendsome
	case "logout":
		err := cli.Logout()
		if err != nil {
			errChan <- fmt.Errorf("Error logging out: %v", err)
			goto sendsome
		}
		log.Infof("Successfully logged out")
		goto sendsome
	case "appstate":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: appstate <types...>")
			goto sendsome
		}
		names := []appstate.WAPatchName{appstate.WAPatchName(args[0])}
		if args[0] == "all" {
			names = []appstate.WAPatchName{
				appstate.WAPatchRegular, appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow,
				appstate.WAPatchCriticalUnblockLow, appstate.WAPatchCriticalBlock,
			}
		}
		resync := len(args) > 1 && args[1] == "resync"
		for _, name := range names {
			err := cli.FetchAppState(name, resync, false)
			if err != nil {
				errChan <- fmt.Errorf("Failed to sync app state: %v", err)
				goto sendsome
			}
		}
		goto sendsome
	case "request-appstate-key":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: request-appstate-key <ids...>")
			goto sendsome
		}
		var keyIDs = make([][]byte, len(args))
		for i, id := range args {
			decoded, err := hex.DecodeString(id)
			if err != nil {
				errChan <- fmt.Errorf("Failed to decode %s as hex: %v", id, err)
				goto sendsome
			}
			keyIDs[i] = decoded
		}
		cli.DangerousInternals().RequestAppStateKeys(context.Background(), keyIDs)
		goto sendsome
	case "whoami":
		output <- fmt.Sprint(cli.DangerousInternals().GetOwnID())
		goto sendsome
	case "unavailable-request":
		if len(args) < 3 {
			errChan <- fmt.Errorf("Usage: unavailable-request <chat JID> <sender JID> <message ID>")
			goto sendsome
		}
		chat, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid chat JID: %s", args[0])
			goto sendsome
		}
		sender, ok := parseJID(args[1])
		if !ok {
			errChan <- fmt.Errorf("Invalid sender JID: %s", args[1])
			goto sendsome
		}
		resp, err := cli.SendMessage(
			context.Background(), cli.Store.ID.ToNonAD(),
			cli.BuildUnavailableMessageRequest(chat, sender, args[2]),
			whatsmeow.SendRequestExtra{Peer: true},
		)
		output <- fmt.Sprintln(resp)
		output <- fmt.Sprintln(err)
		errChan <- err
		goto sendsome
	case "checkuser":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: checkuser <phone numbers...>")
			goto sendsome
		}
		resp, err := cli.IsOnWhatsApp(args)
		if err != nil {
			errChan <- fmt.Errorf("Failed to check if users are on WhatsApp: %v", err)
			goto sendsome
		}
		for _, item := range resp {
			if item.VerifiedName != nil {
				log.Infof("%s: on whatsapp: %t, JID: %s, business name: %s", item.Query, item.IsIn, item.JID, item.VerifiedName.Details.GetVerifiedName())
			} else {
				log.Infof("%s: on whatsapp: %t, JID: %s", item.Query, item.IsIn, item.JID)
			}
		}
		goto sendsome
	case "checkupdate":
		resp, err := cli.CheckUpdate()
		if err != nil {
			errChan <- fmt.Errorf("Failed to check for updates: %v", err)
			goto sendsome
		}
		log.Debugf("Version data: %#v", resp)
		if resp.ParsedVersion == store.GetWAVersion() {
			log.Infof("Client is up to date")
		} else if store.GetWAVersion().LessThan(resp.ParsedVersion) {
			log.Warnf("Client is outdated")
		} else {
			log.Infof("Client is newer than latest")
		}
		goto sendsome
	case "subscribepresence":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: subscribepresence <jid>")
			goto sendsome
		}
		jid, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		}
		err := cli.SubscribePresence(jid)
		if err != nil {
			output <- fmt.Sprintln(err)
			errChan <- err
			goto sendsome
		}
		goto sendsome
	case "presence":
		if len(args) == 0 {
			errChan <- fmt.Errorf("Usage: presence <available/unavailable>")
			goto sendsome
		}
		output <- fmt.Sprint(cli.SendPresence(types.Presence(args[0])))
		goto sendsome
	case "chatpresence":
		if len(args) == 2 {
			args = append(args, "")
		} else if len(args) < 2 {
			errChan <- fmt.Errorf("Usage: chatpresence <jid> <composing/paused> [audio]")
			goto sendsome
		}
		jid, _ := types.ParseJID(args[0])
		output <- fmt.Sprint(cli.SendChatPresence(jid, types.ChatPresence(args[1]), types.ChatPresenceMedia(args[2])))
		goto sendsome
	case "privacysettings":
		resp, err := cli.TryFetchPrivacySettings(false)
		if err != nil {
			output <- fmt.Sprintln(err)
			errChan <- err
			goto sendsome
		}
		output <- fmt.Sprintf("%+v\n", resp)
		goto sendsome
	case "getuser":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: getuser <jids...>")
			goto sendsome
		}
		var jids []types.JID
		for _, arg := range args {
			jid, ok := parseJID(arg)
			if !ok {
				errChan <- fmt.Errorf("Invalid JID: %s", arg)
				goto sendsome
			}
			jids = append(jids, jid)
		}
		resp, err := cli.GetUserInfo(jids)
		if err != nil {
			errChan <- fmt.Errorf("Failed to get user info: %v", err)
			goto sendsome
		}
		for jid, info := range resp {
			log.Infof("%s: %+v", jid, info)
		}
		goto sendsome
	case "mediaconn":
		conn, err := cli.DangerousInternals().RefreshMediaConn(false)
		if err != nil {
			errChan <- fmt.Errorf("Failed to get media connection: %v", err)
			goto sendsome
		}
		log.Infof("Media connection: %+v", conn)
		goto sendsome
	case "getavatar":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: getavatar <jid> [existing ID] [--preview] [--community]")
			goto sendsome
		}
		jid, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
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
			errChan <- fmt.Errorf("Failed to get avatar: %v", err)
			goto sendsome
		} else if pic != nil {
			log.Infof("Got avatar ID %s: %s", pic.ID, pic.URL)
		} else {
			log.Infof("No avatar found")
		}
		goto sendsome
	case "getgroup":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: getgroup <jid>")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		} else if group.Server != types.GroupServer {
			errChan <- fmt.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetGroupInfo(group)
		if err != nil {
			errChan <- fmt.Errorf("Failed to get group info: %v", err)
			goto sendsome
		}
		log.Infof("Group info: %+v", resp)
		goto sendsome
	case "subgroups":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: subgroups <jid>")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		} else if group.Server != types.GroupServer {
			errChan <- fmt.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetSubGroups(group)
		if err != nil {
			errChan <- fmt.Errorf("Failed to get subgroups: %v", err)
			goto sendsome
		}
		for _, sub := range resp {
			log.Infof("Subgroup: %+v", sub)
		}
		goto sendsome
	case "communityparticipants":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: communityparticipants <jid>")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		} else if group.Server != types.GroupServer {
			errChan <- fmt.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetLinkedGroupsParticipants(group)
		if err != nil {
			errChan <- fmt.Errorf("Failed to get community participants: %v", err)
			goto sendsome
		}
		log.Infof("Community participants: %+v", resp)
		goto sendsome
	case "listgroups":
		groups, err := cli.GetJoinedGroups()
		if err != nil {
			errChan <- fmt.Errorf("Failed to get group list: %v", err)
			goto sendsome
		}
		for _, group := range groups {
			log.Infof("%+v", group)
		}
		goto sendsome
	case "getinvitelink":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: getinvitelink <jid> [--reset]")
			goto sendsome
		}
		group, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		} else if group.Server != types.GroupServer {
			errChan <- fmt.Errorf("Input must be a group JID (@%s)", types.GroupServer)
			goto sendsome
		}
		resp, err := cli.GetGroupInviteLink(group, len(args) > 1 && args[1] == "--reset")
		if err != nil {
			errChan <- fmt.Errorf("Failed to get group invite link: %v", err)
			goto sendsome
		}
		log.Infof("Group invite link: %s", resp)
		goto sendsome
	case "queryinvitelink":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: queryinvitelink <link>")
			goto sendsome
		}
		resp, err := cli.GetGroupInfoFromLink(args[0])
		if err != nil {
			errChan <- fmt.Errorf("Failed to resolve group invite link: %v", err)
			goto sendsome
		}
		log.Infof("Group info: %+v", resp)
		goto sendsome
	case "querybusinesslink":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: querybusinesslink <link>")
			goto sendsome
		}
		resp, err := cli.ResolveBusinessMessageLink(args[0])
		if err != nil {
			errChan <- fmt.Errorf("Failed to resolve business message link: %v", err)
			goto sendsome
		}
		log.Infof("Business info: %+v", resp)
		goto sendsome
	case "joininvitelink":
		if len(args) < 1 {
			errChan <- fmt.Errorf("Usage: acceptinvitelink <link>")
			goto sendsome
		}
		groupID, err := cli.JoinGroupWithLink(args[0])
		if err != nil {
			errChan <- fmt.Errorf("Failed to join group via invite link: %v", err)
			goto sendsome
		}
		log.Infof("Joined %s", groupID)
		goto sendsome
	case "getstatusprivacy":
		resp, err := cli.GetStatusPrivacy()
		output <- fmt.Sprintln(err)
		output <- fmt.Sprintln(resp)
		goto sendsome
	case "setdisappeartimer":
		if len(args) < 2 {
			errChan <- fmt.Errorf("Usage: setdisappeartimer <jid> <days>")
			goto sendsome
		}
		days, err := strconv.Atoi(args[1])
		if err != nil {
			errChan <- fmt.Errorf("Invalid duration: %v", err)
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		}
		err = cli.SetDisappearingTimer(recipient, time.Duration(days)*24*time.Hour)
		if err != nil {
			errChan <- fmt.Errorf("Failed to set disappearing timer: %v", err)
			goto sendsome
		}
		goto sendsome
	case "sendpoll":
		if len(args) < 7 {
			errChan <- fmt.Errorf("Usage: sendpoll <jid> <max answers> <question> -- <option 1> / <option 2> / ...")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
			goto sendsome
		}
		maxAnswers, err := strconv.Atoi(args[1])
		if err != nil {
			errChan <- fmt.Errorf("Number of max answers must be an integer")
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
			errChan <- fmt.Errorf("Error sending message: %v", err)
			goto sendsome
		}
		log.Infof("Message sent (server timestamp: %s)", resp.Timestamp)
		goto sendsome
	case "multisend":
		if len(args) < 3 {
			errChan <- fmt.Errorf("Usage: multisend <jids...> -- <text>")
			goto sendsome
		}
		var recipients []types.JID
		for len(args) > 0 && args[0] != "--" {
			recipient, ok := parseJID(args[0])
			args = args[1:]
			if !ok {
				errChan <- fmt.Errorf("Invalid JID: %s", args[0])
				goto sendsome
			}
			recipients = append(recipients, recipient)
		}
		if len(args) == 0 {
			errChan <- fmt.Errorf("Usage: multisend <jids...> -- <text> (the -- is required)")
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
		goto sendsome
	case "react":
		if len(args) < 3 {
			errChan <- fmt.Errorf("Usage: react <jid> <message ID> <reaction>")
			goto sendsome
		}
		recipient, ok := parseJID(args[0])
		if !ok {
			errChan <- fmt.Errorf("Invalid JID: %s", args[0])
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
			errChan <- fmt.Errorf("Error sending reaction: %v", err)
			goto sendsome
		}
		log.Infof("Reaction sent (server timestamp: %s)", resp.Timestamp)
		goto sendsome
	default:
		errChan <- fmt.Errorf("Unknown command: %s", cmd)
		goto sendsome
	}
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
