// Copyright ©2016 Dan Kortschak. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// bit-user-callback is a Back In Time user callback to determine the current
// wi-fi connection and wake a target server using Wake-On-Lan.
//
// The executable or a symlink to the executable link should be placed at
// $XDG_CONFIG_HOME/backintime/user-callback or ~/.config/backintime/user-callback
// if is $XDG_CONFIG_HOME is not set.
// Configuration is read from user-callback.json in the same directory.
//
// See https://github.com/bit-team/user-callback for details of the Back In Time
// user-callback functionality.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kortschak/wol"
)

const (
	// Default iwconfig path
	iwconfig = "/sbin/iwconfig"

	// WOL defaults
	delay   = 20 * time.Second
	timeout = 10 * time.Minute
	remote  = "255.255.255.255:9"

	// mount is the "Mount all necessary drives" reason.
	mount = "7"
)

type config struct {
	Iwconfig string `json:"iwconfig-path"`
	LogFile  string `json:"logfile"`
	Verbose  bool   `json:"verbose"`

	Profile string `json:"profile"`
	ESSID   string `json:"essid"`
	Server  string `json:"server"`

	MAC     string   `json:"wake-mac"`
	Delay   duration `json:"wake-delay"`
	Timeout duration `json:"wake-timeout"`
	Local   string   `json:"wake-local"`
	Remote  string   `json:"wake-remote"`
}

type duration time.Duration

// UnmarshalJSON unmarshals a duration according to the following scheme:
//  * If the element is absent the duration is zero.
//  * If the element is parsable as a time.Duration, the parsed value is kept.
//  * If the element is parsable as a number, that number of seconds is kept.
func (d *duration) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		*d = 0
		return nil
	}
	text, err := strconv.Unquote(string(data))
	if err != nil {
		return err
	}
	t, err := time.ParseDuration(text)
	if err == nil {
		*d = duration(t)
		return nil
	}
	i, err := strconv.ParseInt(text, 10, 64)
	if err == nil {
		*d = duration(time.Duration(i) * time.Second)
		return nil
	}
	// This hack is to get around strconv.ParseInt
	// not handling e-notation for integers.
	f, err := strconv.ParseFloat(text, 64)
	*d = duration(time.Duration(f) * time.Second)
	return err
}

// MarshalJSON marshals a duration according as Go formatted time.Duration.
func (d duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(time.Duration(d).String())), nil
}

// installLink creates a symbolic link from the Back In Time config directory
// to the executable.
func installLink() {
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		log.Fatalf("could not determine executable path: %v", err)
	}
	dir, err := configDir()
	if err != nil {
		log.Fatalf("could not determine config directory: %v", err)
	}
	err = os.Symlink(exe, filepath.Join(dir, "user-callback"))
	if err != nil {
		log.Fatalf("could not create symbolic link: %v", err)
	}
}

// generateConfig writes a default configuration file.
func generateConfig() {
	dir, err := configDir()
	if err != nil {
		log.Fatalf("could not determine config directory: %v", err)
	}
	path := filepath.Join(dir, "user-callback.json")

	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("failed to create config file: %v", err)
	}
	defer f.Close()

	c := config{
		Iwconfig: iwconfig,
		Delay:    duration(delay),
		Timeout:  duration(timeout),
		Remote:   remote,
	}
	if p, err := exec.LookPath("iwconfig"); err == nil {
		c.Iwconfig = p
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		log.Fatalf("failed to marshal configuration: %v", err)
	}
	_, err = f.Write(b)
	if err != nil {
		log.Fatalf("failed to write configuration: %v", err)
	}

	fmt.Printf("wrote configuration file to %q\n", path)
}

// readConfig returns the configuration for user-callback.
func readConfig() (*config, error) {
	dir, err := configDir()
	if err != nil {
		return nil, fmt.Errorf("could not determine config directory: %v", err)
	}
	path := filepath.Join(dir, "user-callback.json")

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %v", err)
	}
	defer f.Close()

	var c config
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &c)
	if err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}
	return &c, nil
}

// configDir returns the location of the backintime config directory.
func configDir() (string, error) {
	dir, ok := os.LookupEnv("XDG_CONFIG_HOME")
	if ok {
		return filepath.Join(dir, "backintime"), nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(u.HomeDir, ".config", "backintime"), nil
}

// essids returns the ESSIDS of wireless interfaces that the host is connected to.
func essids() ([]string, error) {
	const essid = "ESSID:"

	cmd := exec.Command(iwconfig)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	var essids []string
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		if i := bytes.Index(b, []byte(essid)); i != -1 {
			s := string(b[i+len(essid):])
			id, err := strconv.Unquote(s)
			if err != nil {
				return essids, fmt.Errorf("%v: %q", err, s)
			}
			essids = append(essids, id)
		}
	}
	return essids, nil
}

// contains returns whether s matches an element of slice.
func contains(s string, slice []string) bool {
	for _, e := range slice {
		if s == e {
			return true
		}
	}
	return false
}

// wake sends a WOL package to the remote address via the local interface, targeting
// the given mac address.
func wake(mac, local, remote string) error {
	raddr, err := net.ResolveUDPAddr("udp", remote)
	if err != nil {
		return fmt.Errorf("could not parse remote %q as a valid UDP address: %v\n", remote, err)
	}
	var laddr *net.UDPAddr
	if local != "" {
		laddr, err = net.ResolveUDPAddr("udp", local)
		if err != nil {
			return fmt.Errorf("could not parse local %q as a valid UDP address: %v\n", local, err)
		}
	}

	hwaddr, err := net.ParseMAC(mac)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not parse %q as a valid MAC address: %v\n", mac, err)
	}
	err = wol.Wake(hwaddr, nil, laddr, raddr)
	if err != nil {
		return fmt.Errorf("error attempting to wake %s: %v\n", hwaddr, err)
	}
	return nil
}

func main() {
	genconf := flag.Bool("genconf", false, "generate a configuration file")
	install := flag.Bool("install", false, "create a symlink to the executable")
	help := flag.Bool("help", false, "print this message")
	flag.Parse()
	if *help {
		fmt.Fprintln(os.Stderr, `Usage of bit-user-callback:

If invoked by Back In Time, user-callback accepts three or more arguments:

* the profile id (1=Main Profile, ...)
* the profile name
* the reason as described at [1]

user-callback ignores profile id and only acts for reason 7.

Operation of user-callback is configured via a JSON file. A default
configuration will be written by invoking bit-user-callback with -genconf.

[1]https://github.com/bit-team/user-callback
`)
		flag.PrintDefaults()
		os.Exit(0)
	}
	if *install {
		installLink()
	}
	if *genconf {
		generateConfig()
	}
	if *install || *genconf {
		os.Exit(0)
	}

	info := log.New(os.Stdout, "user-callback: ", log.LstdFlags)
	fatal := log.New(os.Stderr, "user-callback: ", log.LstdFlags)

	c, err := readConfig()
	if err != nil {
		fatal.Fatalf("failed to read config: %v", err)
	}

	var f *os.File
	if c.LogFile != "" {
		f, err = os.OpenFile(c.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fatal.Fatal(err)
		}
		defer f.Close()
		info.SetOutput(io.MultiWriter(os.Stdout, f))
		fatal.SetOutput(io.MultiWriter(os.Stderr, f))
	}

	if c.Verbose {
		info.Printf("received arguments: %q", flag.Args())
	}
	if flag.NArg() < 3 {
		fatal.Fatalf("unexpected number of arguments: want >=3, got %d", flag.NArg())
	}
	profile := flag.Args()[1]
	reason := flag.Args()[2]
	if profile != c.Profile || reason != mount {
		return
	}

	ssids, err := essids()
	if err != nil {
		fatal.Fatal(err)
	}
	if !contains(c.ESSID, ssids) {
		info.Fatalf("not connected to %q", c.ESSID)
	}

	start := time.Now()
	var sent bool
	for {
		if time.Since(start) > time.Duration(c.Timeout) {
			fatal.Fatal("timed out waiting for %s", c.Server)
		}
		resp, err := http.Get(c.Server)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if !sent {
			info.Print("sending wake packet")
			err = wake(c.MAC, c.Local, c.Remote)
			if err != nil {
				fatal.Fatal(err)
			}
			sent = true
		}
		time.Sleep(time.Duration(c.Delay))
	}
	info.Print("server ready")
}
