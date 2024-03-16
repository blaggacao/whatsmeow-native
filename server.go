// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type Command struct {
	Cmd  string
	Args []string
}

func commandHandler(w http.ResponseWriter, r *http.Request) {
	var cmd Command
	err := json.NewDecoder(r.Body).Decode(&cmd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	handleCmd(strings.ToLower(cmd.Cmd), cmd.Args)

	w.WriteHeader(http.StatusOK)
}

func makeServer(addr string) *http.Server {
	server := &http.Server{
		Addr: addr,
	}
	http.HandleFunc("/command", commandHandler)
	return server
}
