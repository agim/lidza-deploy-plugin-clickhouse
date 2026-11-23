package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// machine is the server the plugin changes. Tests point root at a directory
// and replace the commands, the SQL endpoint, the download and the accounts.
type machine struct {
	root      string
	run       func(ctx context.Context, name string, args ...string) (string, error)
	sql       func(ctx context.Context, port int, user, password, statement string) (string, error)
	download  func(ctx context.Context, url string) ([]byte, error)
	portTaken func(port int) bool
	// account answers the uid and gid of the clickhouse user.
	account func() (uid, gid int, err error)
	chown   bool
}

func newMachine() *machine {
	return &machine{run: realHost{}.Run, sql: httpSQL, download: downloadKey, portTaken: listening, account: clickhouseAccount, chown: os.Geteuid() == 0}
}

func (m *machine) path(path string) string {
	if m.root == "" {
		return path
	}
	return filepath.Join(m.root, path)
}

func (m *machine) exists(path string) bool {
	_, err := os.Stat(m.path(path))
	return err == nil
}

// write replaces a file when its content or mode differs, atomically, and
// reports whether it changed.
func (m *machine) write(path string, data []byte, mode os.FileMode, uid, gid int) (bool, error) {
	full := m.path(path)
	if old, err := os.ReadFile(full); err == nil && bytes.Equal(old, data) {
		if info, err := os.Stat(full); err == nil && info.Mode().Perm() == mode {
			if m.chown {
				_ = os.Chown(full, uid, gid)
			}
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".lidza-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil && m.chown {
		err = tmp.Chown(uid, gid)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), full)
}

// mkdir makes a directory for the clickhouse user.
func (m *machine) mkdir(path string, uid, gid int) error {
	full := m.path(path)
	if err := os.MkdirAll(full, 0o750); err != nil {
		return err
	}
	if m.chown {
		return os.Chown(full, uid, gid)
	}
	return nil
}

// copyTree copies the regular files and directories of src into dst, which
// must not exist, for owner uid:gid. Anything else in src is refused.
func (m *machine) copyTree(src, dst string, uid, gid int) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0o750); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
			if err != nil {
				in.Close()
				return err
			}
			_, err = io.Copy(out, in)
			in.Close()
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not a regular file", rel)
		}
		if m.chown {
			return os.Chown(target, uid, gid)
		}
		return nil
	})
}

// moveTree moves a directory, copying when the two are on different file
// systems.
func (m *machine) moveTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := m.copyTree(src, dst, 0, 0); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// ---- the real machine ---------------------------------------------------------------

func listening(port int) bool {
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return true
	}
	l.Close()
	return false
}

func clickhouseAccount() (int, int, error) {
	account, err := user.Lookup("clickhouse")
	if err != nil {
		return 0, 0, errors.New("the clickhouse user does not exist on this server")
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(account.Gid)
	return uid, gid, err
}

func downloadKey(ctx context.Context, url string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(bounded, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading the signing key: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading the signing key: HTTP %d", res.StatusCode)
	}
	key, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(key, []byte("BEGIN PGP PUBLIC KEY BLOCK")) {
		return nil, errors.New("the downloaded signing key is not an armored OpenPGP key")
	}
	return key, nil
}

// httpSQL runs one statement over ClickHouse's loopback HTTP interface and
// returns its output. The password never appears in an error.
func httpSQL(ctx context.Context, port int, user, password, statement string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/", strings.NewReader(statement))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-ClickHouse-User", user)
	req.Header.Set("X-ClickHouse-Key", password)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("ClickHouse is unreachable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		text := strings.TrimSpace(string(detail))
		if password != "" {
			text = strings.ReplaceAll(text, password, "***")
		}
		return "", fmt.Errorf("ClickHouse answered %d: %s", res.StatusCode, text)
	}
	out, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return strings.TrimRight(string(out), "\n"), err
}
