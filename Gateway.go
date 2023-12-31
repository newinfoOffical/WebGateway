/*
File Name:  Gateway.go
Copyright:  2022 Peernet s.r.o.
Author:     Peter Kleissner
*/

package main

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"github.com/Akilan1999/p2p-rendering-computation/p2p/frp"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/PeernetOfficial/core"
	"github.com/PeernetOfficial/core/btcec"
	"github.com/PeernetOfficial/core/webapi"
	"github.com/gorilla/mux"
)

func startWebGateway(backend *core.Backend) {
	router := mux.NewRouter()

	router.PathPrefix("/static/").Handler(
		http.StripPrefix("/static/",
			http.FileServer(
				http.Dir("./html"),
			),
		),
	)

	router.PathPrefix("/").Handler(http.HandlerFunc(webGatewayHandler(backend))).Methods("GET")

	for _, listen := range config.WebListen {
		go startWebServer(backend, listen, config.WebUseSSL, config.WebCertificateFile, config.WebCertificateKey, router, "Web Listen", parseDuration(config.WebTimeoutRead), parseDuration(config.WebTimeoutWrite))
	}

	if config.Redirect80 != "" {
		go webRedirect80(config.Redirect80)
	}

	// wait forever
	select {}
}

// startWebServer starts the web-server and may block forever
func startWebServer(backend *core.Backend, WebListen string, UseSSL bool, CertificateFile, CertificateKey string, Handler http.Handler, Info string, ReadTimeout, WriteTimeout time.Duration) {
	//func startWebServer(backend *core.Backend, webListen string, useSSL bool, certificateFile, certificateKey string, server *http.Server) {
	backend.LogError("startWebServer", "Web Gateway to listen on '%s'", WebListen)

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12} // for security reasons disable TLS 1.0/1.1

	server := &http.Server{
		Addr:         WebListen,
		Handler:      Handler,
		ReadTimeout:  ReadTimeout,  // ReadTimeout is the maximum duration for reading the entire request, including the body.
		WriteTimeout: WriteTimeout, // WriteTimeout is the maximum duration before timing out writes of the response. This includes processing time and is therefore the max time any HTTP function may take.
		//IdleTimeout:  IdleTimeout,  // IdleTimeout is the maximum amount of time to wait for the next request when keep-alives are enabled.
		TLSConfig: tlsConfig,
	}

	if UseSSL {
		// HTTPS
		if err := server.ListenAndServeTLS(CertificateFile, CertificateKey); err != nil {
			backend.LogError("startWebServer", "Error listening on '%s': %v\n", WebListen, err)
		}
	} else {
		// HTTP
		if err := server.ListenAndServe(); err != nil {
			backend.LogError("startWebServer", "Error listening on '%s': %v\n", WebListen, err)
		}
	}
}

// parseDuration is the same as time.ParseDuration without returning an error. Valid units are ms, s, m, h. For example "10s".
func parseDuration(input string) (result time.Duration) {
	result, _ = time.ParseDuration(input)
	return
}

func redirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
}

func webRedirect80(listen80 string) {
	// redirect HTTP -> HTTPS
	http.ListenAndServe(net.JoinHostPort(listen80, "80"), http.HandlerFunc(redirect))
}

func webGatewayHandler(backend *core.Backend) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// For security and simplicity reasons, below paths are hard-coded.
		// Using arbitrary user input is an avoidable security risk here.
		switch r.URL.Path {
		case "/", "/index.html":
			http.ServeFile(w, r, path.Join(config.WebFiles, "index.html"))
			return
		case "/favicon.ico":
			http.ServeFile(w, r, path.Join(config.WebFiles, "favicon.ico"))
			return
		case "/download":
			http.ServeFile(w, r, path.Join(config.WebFiles, "download.html"))
			return
		}

		// Assume it is in the format "/[blockchain public key]/[file hash]".

		// Remove slash prefix and suffix.
		pathA := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/")

		pathParts := strings.Split(pathA, "/")
		if len(pathParts) != 1 && len(pathParts) != 2 {
			http.Error(w, "404 not found", http.StatusNotFound)
			return
		}

		// Default timeout for connection is 10 seconds. This will be an optional parameter in the future.
		timeout := 10 * time.Second

		// First part must be the public key as peer ID or node ID, hex encoded. Form: "/[blockchain public key]"
		nodeIDA := pathParts[0]
		nodeID, validNodeID := webapi.DecodeBlake3Hash(nodeIDA)
		publicKey, errPK := core.PublicKeyFromPeerID(nodeIDA)

		if !validNodeID && errPK != nil {
			http.Error(w, "Invalid NodeID", http.StatusBadRequest)
			return
		}
		if !validNodeID {
			nodeID = []byte{}
		}

		// Check if a blockchain is requested.
		// The format must be "/[blockchain public key]". Part 1 = hex encoding, peer ID or node ID.
		if len(pathParts) == 1 {
			webGatewayShowBlockchain(backend, w, r, nodeID, publicKey, timeout)
		} else if len(pathParts) == 2 {
			// Check if a specific file on a specific blockchain is requested.
			// The format must be "/[blockchain public key]/[file hash]". Part 2 = hex encoding, blake3 hash.
			hash, valid := webapi.DecodeBlake3Hash(pathParts[1])
			if !valid {
				http.Error(w, "Invalid file hash.", http.StatusBadRequest)
				return
			}

			webGatewayShowFile(backend, w, r, nodeID, publicKey, hash, timeout)
		}

		// Check if an arbitrary directory on a specific blockchain is requested.
		// Directories are identified by name.
		// TODO

		//http.Error(w, "test handler", http.StatusOK)
	}
}

func webGatewayShowBlockchain(backend *core.Backend, w http.ResponseWriter, r *http.Request, nodeID []byte, publicKey *btcec.PublicKey, timeout time.Duration) {
	var err error
	var peer *core.PeerInfo
	if len(nodeID) != 0 {
		peer, err = webapi.PeerConnectNode(backend, nodeID, timeout)
	} else {
		peer, err = webapi.PeerConnectPublicKey(backend, publicKey, timeout)
	}
	if err != nil {
		http.Error(w, "Could not connect to remote peer. 😢", http.StatusNotFound)
		return
	}

	// connection established!
	text := fmt.Sprintf("Peer %s blockchain height %d version %d\nUser Agent: %s\n", hex.EncodeToString(peer.NodeID), peer.BlockchainHeight, peer.BlockchainVersion, peer.UserAgent)
	http.Error(w, text, http.StatusOK)
}

// MetaDataResponse meta data struct
type MetaDataResponse struct {
	Name string
	Hash []byte
	Size uint64
}

func webGatewayShowFile(backend *core.Backend, w http.ResponseWriter, r *http.Request, nodeID []byte, publicKey *btcec.PublicKey, fileHash []byte, timeout time.Duration) {
	var err error
	var peer *core.PeerInfo
	if len(nodeID) != 0 {
		peer, err = webapi.PeerConnectNode(backend, nodeID, timeout)
	} else {
		peer, err = webapi.PeerConnectPublicKey(backend, publicKey, timeout)
	}
	if err != nil {
		http.Error(w, "Could not connect to remote peer. 😢", http.StatusNotFound)
		return
	}

	// Todo: Try webapi.serveFileFromWarehouse

	// Todo: Cache.

	offset := 0
	limit := 0

	// start the reader
	//reader, fileSize, transferSize, err := webapi.FileStartReader(peer, fileHash, uint64(offset), uint64(limit), r.Context().Done())
	reader, fileSize, transferSize, err := webapi.FileStartReader(peer, fileHash, uint64(offset), uint64(limit), r.Context().Done())
	if reader != nil {
		defer reader.Close()
	}
	if err != nil || reader == nil {
		http.Error(w, "File not found.", http.StatusNotFound)
		return
	}

	// if get request ?download=true
	// trigger download of the file
	download := r.URL.Query().Get("download")
	metadata := r.URL.Query().Get("metadata")
	filename := r.URL.Query().Get("filename")
	play := r.URL.Query().Get("play")

	//// look up file based on NodeID
	//var resultMap map[uuid.UUID]*search.SearchIndexRecord
	//data, _ := peer.Backend.SearchIndex.Database.Get(fileHash)
	//err = peer.Backend.SearchIndex.LookupHash(search.SearchSelector{Hash: fileHash}, resultMap)
	//if err != nil {
	//	fmt.Println(err)
	//}
	//
	//var fileIndexRecord search.SearchIndexRecord
	//// looping over results and ensuring the public key matches
	//// This is to ensure the correct NodeID matches with the correct hash
	//for i, _ := range resultMap {
	//	if resultMap[i].PublicKey == peer.PublicKey {
	//		resultMap[i] = &fileIndexRecord
	//		break
	//	}
	//}

	//// Get information from the blockchain about a particular file
	//file, _, found, err := backend.ReadFile(peer.PublicKey, peer.BlockchainVersion, fileIndexRecord.BlockNumber, fileIndexRecord.FileID)
	//if err != nil {
	//	fmt.Println(err)
	//	fmt.Println(found)
	//}

	var responseMetadata MetaDataResponse

	//// Get name from file information
	//for i, _ := range file.Tags {
	//	// This is for the file name
	//	if blockchain.TagName == file.Tags[i].Type {
	//		responseMetadata.Name = file.Tags[i].Text()
	//	}
	//}

	if filename != "" {
		responseMetadata.Name = filename
	}

	if download == "true" {
		w.Header().Set("Content-Disposition", "attachment; filename="+responseMetadata.Name)
		// JSON response with meta data instead
		io.Copy(w, io.LimitReader(reader, int64(transferSize)))
		return
	} else if metadata == "true" {
		// JSON response of the file meta-data
		responseMetadata.Size = fileSize
		responseMetadata.Hash = fileHash

		webapi.EncodeJSON(backend, w, r, responseMetadata)
		return
	} else if play == "true" {
		// Plays / previews the video
		io.Copy(w, io.LimitReader(reader, int64(transferSize)))
	}

	// set the right headers
	//webapi.setContentLengthRangeHeader(w, uint64(offset), transferSize, fileSize, ranges)

	http.ServeFile(w, r, path.Join(config.WebFiles, "download.html"))

	// Start sending the data!
	//io.Copy(w, io.LimitReader(reader, int64(transferSize)))
}

func EscapeNATWebGateway() (ExposePortP2PRC string, err error) {
	host, port, err := net.SplitHostPort(config.P2PRCRootPeer)
	if err != nil {
		return
	}

	serverPort, err := frp.GetFRPServerPort("http://" + host + ":" + port)

	if err != nil {
		return
	}

	time.Sleep(1 * time.Second)

	_, port, err = net.SplitHostPort(config.WebListen[0])
	if err != nil {
		return
	}

	//port for the barrierKVM server
	ExposePortP2PRC, err = frp.StartFRPClientForServer(host, serverPort, port, config.ExposePortP2PRC)
	if err != nil {
		return
	}

	return
}
