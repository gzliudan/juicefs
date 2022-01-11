/*
 * JuiceFS, Copyright 2018 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/sync"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

func supportHTTPS(name, endpoint string) bool {
	switch name {
	case "ufile":
		return !(strings.Contains(endpoint, ".internal-") || strings.HasSuffix(endpoint, ".ucloud.cn"))
	case "oss":
		return !(strings.Contains(endpoint, ".vpc100-oss") || strings.Contains(endpoint, "internal.aliyuncs.com"))
	case "jss":
		return false
	case "s3":
		ps := strings.SplitN(strings.Split(endpoint, ":")[0], ".", 2)
		if len(ps) > 1 && net.ParseIP(ps[1]) != nil {
			return false
		}
	case "minio":
		return false
	}
	return true
}

// Check if uri is local file path
func isFilePath(uri string) bool {
	// check drive pattern when running on Windows
	if runtime.GOOS == "windows" &&
		len(uri) > 1 && (('a' <= uri[0] && uri[0] <= 'z') ||
		('A' <= uri[0] && uri[0] <= 'Z')) && uri[1] == ':' {
		return true
	}
	return !strings.Contains(uri, ":")
}

func createSyncStorage(uri string, conf *sync.Config) (object.ObjectStorage, error) {
	if !strings.Contains(uri, "://") {
		if isFilePath(uri) {
			absPath, err := filepath.Abs(uri)
			if err != nil {
				logger.Fatalf("invalid path: %s", err.Error())
			}
			if !strings.HasPrefix(absPath, "/") { // Windows path
				absPath = "/" + strings.Replace(absPath, "\\", "/", -1)
			}
			if strings.HasSuffix(uri, "/") {
				absPath += "/"
			}

			// Windows: file:///C:/a/b/c, Unix: file:///a/b/c
			uri = "file://" + absPath
		} else { // sftp
			var user string
			if strings.Contains(uri, "@") {
				parts := strings.Split(uri, "@")
				user = parts[0]
				uri = parts[1]
			}
			var pass string
			if strings.Contains(user, ":") {
				parts := strings.Split(user, ":")
				user = parts[0]
				pass = parts[1]
			} else if os.Getenv("SSH_PRIVATE_KEY_PATH") == "" {
				fmt.Print("Enter Password: ")
				bytePassword, err := term.ReadPassword(int(syscall.Stdin))
				if err != nil {
					logger.Fatalf("Read password: %s", err.Error())
				}
				pass = string(bytePassword)
			}
			return object.CreateStorage("sftp", uri, user, pass)
		}
	}
	u, err := url.Parse(uri)
	if err != nil {
		logger.Fatalf("Can't parse %s: %s", uri, err.Error())
	}
	user := u.User
	var accessKey, secretKey string
	if user != nil {
		accessKey = user.Username()
		secretKey, _ = user.Password()
	}
	name := strings.ToLower(u.Scheme)
	endpoint := u.Host

	isS3PathTypeUrl := isS3PathType(endpoint)

	if name == "file" {
		endpoint = u.Path
	} else if name == "hdfs" {
	} else if !conf.NoHTTPS && supportHTTPS(name, endpoint) {
		endpoint = "https://" + endpoint
	} else {
		endpoint = "http://" + endpoint
	}
	if name == "minio" || name == "s3" && isS3PathTypeUrl {
		// bucket name is part of path
		endpoint += u.Path
	}

	store, err := object.CreateStorage(name, endpoint, accessKey, secretKey)
	if err != nil {
		return nil, fmt.Errorf("create %s %s: %s", name, endpoint, err)
	}
	if conf.Perms {
		if _, ok := store.(object.FileSystem); !ok {
			logger.Warnf("%s is not a file system, can not preserve permissions", store)
			conf.Perms = false
		}
	}
	switch name {
	case "file":
	case "minio":
		if strings.Count(u.Path, "/") > 1 {
			// skip bucket name
			store = object.WithPrefix(store, strings.SplitN(u.Path[1:], "/", 2)[1])
		}
	case "s3":
		if isS3PathTypeUrl && strings.Count(u.Path, "/") > 1 {
			store = object.WithPrefix(store, strings.SplitN(u.Path[1:], "/", 2)[1])
		} else if len(u.Path) > 1 {
			store = object.WithPrefix(store, u.Path[1:])
		}
	default:
		if len(u.Path) > 1 {
			store = object.WithPrefix(store, u.Path[1:])
		}
	}
	return store, nil
}

func isS3PathType(endpoint string) bool {
	//localhost[:8080] 127.0.0.1[:8080]  s3.ap-southeast-1.amazonaws.com[:8080] s3-ap-southeast-1.amazonaws.com[:8080]
	pattern := `^((localhost)|(s3[.-].*\.amazonaws\.com)|((1\d{2}|2[0-4]\d|25[0-5]|[1-9]\d|[1-9])\.((1\d{2}|2[0-4]\d|25[0-5]|[1-9]\d|\d)\.){2}(1\d{2}|2[0-4]\d|25[0-5]|[1-9]\d|\d)))?(:\d*)?$`
	return regexp.MustCompile(pattern).MatchString(endpoint)
}

const USAGE = `juicefs [options] sync [options] SRC DST
SRC and DST should be [NAME://][ACCESS_KEY:SECRET_KEY@]BUCKET[.ENDPOINT][/PREFIX]`

func doSync(c *cli.Context) error {
	setLoggerLevel(c)

	if c.Args().Len() != 2 {
		logger.Errorf(USAGE)
		return nil
	}
	config := sync.NewConfigFromCli(c)
	go func() { _ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", config.HTTPPort), nil) }()

	// Windows support `\` and `/` as its separator, Unix only use `/`
	srcURL := strings.Replace(c.Args().Get(0), "\\", "/", -1)
	dstURL := strings.Replace(c.Args().Get(1), "\\", "/", -1)
	if strings.HasSuffix(srcURL, "/") != strings.HasSuffix(dstURL, "/") {
		logger.Fatalf("SRC and DST should both end with path separator or not!")
	}
	src, err := createSyncStorage(srcURL, config)
	if err != nil {
		return err
	}
	dst, err := createSyncStorage(dstURL, config)
	if err != nil {
		return err
	}
	return sync.Sync(src, dst, config)
}

func syncFlags() *cli.Command {
	return &cli.Command{
		Name:      "sync",
		Usage:     "sync between two storage",
		ArgsUsage: "SRC DST",
		Action:    doSync,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "start",
				Aliases: []string{"s"},
				Value:   "",
				Usage:   "the first `KEY` to sync",
			},
			&cli.StringFlag{
				Name:    "end",
				Aliases: []string{"e"},
				Value:   "",
				Usage:   "the last `KEY` to sync",
			},
			&cli.IntFlag{
				Name:    "threads",
				Aliases: []string{"p"},
				Value:   10,
				Usage:   "number of concurrent threads",
			},
			&cli.IntFlag{
				Name:  "http-port",
				Value: 6070,
				Usage: "HTTP `PORT` to listen to",
			},
			&cli.BoolFlag{
				Name:    "update",
				Aliases: []string{"u"},
				Usage:   "skip files if the destination is newer",
			},
			&cli.BoolFlag{
				Name:    "force-update",
				Aliases: []string{"f"},
				Usage:   "always update existing files",
			},
			&cli.BoolFlag{
				Name:  "perms",
				Usage: "preserve permissions",
			},
			&cli.BoolFlag{
				Name:  "dirs",
				Usage: "Sync directories or holders",
			},
			&cli.BoolFlag{
				Name:  "dry",
				Usage: "Don't copy file",
			},
			&cli.BoolFlag{
				Name:    "delete-src",
				Aliases: []string{"deleteSrc"},
				Usage:   "delete objects from source those already exist in destination",
			},
			&cli.BoolFlag{
				Name:    "delete-dst",
				Aliases: []string{"deleteDst"},
				Usage:   "delete extraneous objects from destination",
			},
			&cli.StringSliceFlag{
				Name:  "exclude",
				Usage: "exclude keys containing `PATTERN` (POSIX regular expressions)",
			},
			&cli.StringSliceFlag{
				Name:  "include",
				Usage: "only include keys containing `PATTERN` (POSIX regular expressions)",
			},
			&cli.StringFlag{
				Name:  "manager",
				Usage: "manager address",
			},
			&cli.StringSliceFlag{
				Name:  "worker",
				Usage: "hosts (seperated by comma) to launch worker",
			},
			&cli.IntFlag{
				Name:  "bwlimit",
				Usage: "limit bandwidth in Mbps (0 means unlimited)",
			},
			&cli.BoolFlag{
				Name:  "no-https",
				Usage: "donot use HTTPS",
			},
			&cli.BoolFlag{
				Name:  "check-all",
				Usage: "verify integrity of all files in source and destination",
			},
			&cli.BoolFlag{
				Name:  "check-new",
				Usage: "verify integrity of newly copied files",
			},
		},
	}
}
