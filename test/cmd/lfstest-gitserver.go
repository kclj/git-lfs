// +build testtools

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	repoDir      string
	largeObjects = newLfsStorage()
	server       *httptest.Server
	serverTLS    *httptest.Server

	// maps OIDs to content strings. Both the LFS and Storage test servers below
	// see OIDs.
	oidHandlers map[string]string

	// These magic strings tell the test lfs server change their behavior so the
	// integration tests can check those use cases. Tests will create objects with
	// the magic strings as the contents.
	//
	//   printf "status:lfs:404" > 404.dat
	//
	contentHandlers = []string{
		"status-batch-403", "status-batch-404", "status-batch-410", "status-batch-422", "status-batch-500",
		"status-storage-403", "status-storage-404", "status-storage-410", "status-storage-422", "status-storage-500",
		"status-legacy-404", "status-legacy-410", "status-legacy-422", "status-legacy-403", "status-legacy-500",
		"status-batch-resume-206", "batch-resume-fail-fallback",
	}
)

func main() {
	repoDir = os.Getenv("LFSTEST_DIR")

	mux := http.NewServeMux()
	server = httptest.NewServer(mux)
	serverTLS = httptest.NewTLSServer(mux)

	stopch := make(chan bool)

	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		stopch <- true
	})

	mux.HandleFunc("/storage/", storageHandler)
	mux.HandleFunc("/redirect307/", redirect307Handler)
	mux.HandleFunc("/locks", locksHandler)
	mux.HandleFunc("/locks/", locksHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/info/lfs") {
			if !skipIfBadAuth(w, r) {
				lfsHandler(w, r)
			}

			return
		}

		log.Printf("git http-backend %s %s\n", r.Method, r.URL)
		gitHandler(w, r)
	})

	urlname := writeTestStateFile([]byte(server.URL), "LFSTEST_URL", "lfstest-gitserver")
	defer os.RemoveAll(urlname)

	sslurlname := writeTestStateFile([]byte(serverTLS.URL), "LFSTEST_SSL_URL", "lfstest-gitserver-ssl")
	defer os.RemoveAll(sslurlname)

	block := &pem.Block{}
	block.Type = "CERTIFICATE"
	block.Bytes = serverTLS.TLS.Certificates[0].Certificate[0]
	pembytes := pem.EncodeToMemory(block)
	certname := writeTestStateFile(pembytes, "LFSTEST_CERT", "lfstest-gitserver-cert")
	defer os.RemoveAll(certname)

	log.Println(server.URL)
	log.Println(serverTLS.URL)

	<-stopch
	log.Println("git server done")
}

// writeTestStateFile writes contents to either the file referenced by the
// environment variable envVar, or defaultFilename if that's not set. Returns
// the filename that was used
func writeTestStateFile(contents []byte, envVar, defaultFilename string) string {
	f := os.Getenv(envVar)
	if len(f) == 0 {
		f = defaultFilename
	}
	file, err := os.Create(f)
	if err != nil {
		log.Fatalln(err)
	}
	file.Write(contents)
	file.Close()
	return f
}

type lfsObject struct {
	Oid     string             `json:"oid,omitempty"`
	Size    int64              `json:"size,omitempty"`
	Actions map[string]lfsLink `json:"actions,omitempty"`
	Err     *lfsError          `json:"error,omitempty"`
}

type lfsLink struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

type lfsError struct {
	Code    int
	Message string
}

// handles any requests with "{name}.server.git/info/lfs" in the path
func lfsHandler(w http.ResponseWriter, r *http.Request) {
	repo, err := repoFromLfsUrl(r.URL.Path)
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}

	log.Printf("git lfs %s %s repo: %s\n", r.Method, r.URL, repo)
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	switch r.Method {
	case "POST":
		if strings.HasSuffix(r.URL.String(), "batch") {
			lfsBatchHandler(w, r, repo)
		} else {
			lfsPostHandler(w, r, repo)
		}
	case "GET":
		lfsGetHandler(w, r, repo)
	default:
		w.WriteHeader(405)
	}
}

func lfsUrl(repo, oid string) string {
	return server.URL + "/storage/" + oid + "?r=" + repo
}

func lfsPostHandler(w http.ResponseWriter, r *http.Request, repo string) {
	buf := &bytes.Buffer{}
	tee := io.TeeReader(r.Body, buf)
	obj := &lfsObject{}
	err := json.NewDecoder(tee).Decode(obj)
	io.Copy(ioutil.Discard, r.Body)
	r.Body.Close()

	log.Println("REQUEST")
	log.Println(buf.String())
	log.Printf("OID: %s\n", obj.Oid)
	log.Printf("Size: %d\n", obj.Size)

	if err != nil {
		log.Fatal(err)
	}

	switch oidHandlers[obj.Oid] {
	case "status-legacy-403":
		w.WriteHeader(403)
		return
	case "status-legacy-404":
		w.WriteHeader(404)
		return
	case "status-legacy-410":
		w.WriteHeader(410)
		return
	case "status-legacy-422":
		w.WriteHeader(422)
		return
	case "status-legacy-500":
		w.WriteHeader(500)
		return
	}

	res := &lfsObject{
		Oid:  obj.Oid,
		Size: obj.Size,
		Actions: map[string]lfsLink{
			"upload": lfsLink{
				Href:   lfsUrl(repo, obj.Oid),
				Header: map[string]string{},
			},
		},
	}

	if testingChunkedTransferEncoding(r) {
		res.Actions["upload"].Header["Transfer-Encoding"] = "chunked"
	}

	by, err := json.Marshal(res)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("RESPONSE: 202")
	log.Println(string(by))

	w.WriteHeader(202)
	w.Write(by)
}

func lfsGetHandler(w http.ResponseWriter, r *http.Request, repo string) {
	parts := strings.Split(r.URL.Path, "/")
	oid := parts[len(parts)-1]

	// Support delete for testing
	if len(parts) > 1 && parts[len(parts)-2] == "delete" {
		largeObjects.Delete(repo, oid)
		log.Println("DELETE:", oid)
		w.WriteHeader(200)
		return
	}

	by, ok := largeObjects.Get(repo, oid)
	if !ok {
		w.WriteHeader(404)
		return
	}

	obj := &lfsObject{
		Oid:  oid,
		Size: int64(len(by)),
		Actions: map[string]lfsLink{
			"download": lfsLink{
				Href: lfsUrl(repo, oid),
			},
		},
	}

	by, err := json.Marshal(obj)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("RESPONSE: 200")
	log.Println(string(by))

	w.WriteHeader(200)
	w.Write(by)
}

func lfsBatchHandler(w http.ResponseWriter, r *http.Request, repo string) {
	if repo == "batchunsupported" {
		w.WriteHeader(404)
		return
	}

	if repo == "badbatch" {
		w.WriteHeader(203)
		return
	}

	if repo == "netrctest" {
		user, pass, err := extractAuth(r.Header.Get("Authorization"))
		if err != nil || (user != "netrcuser" || pass != "netrcpass") {
			w.WriteHeader(403)
			return
		}
	}

	if missingRequiredCreds(w, r, repo) {
		return
	}

	type batchReq struct {
		Transfers []string    `json:"transfers"`
		Operation string      `json:"operation"`
		Objects   []lfsObject `json:"objects"`
	}
	type batchResp struct {
		Transfer string      `json:"transfer,omitempty"`
		Objects  []lfsObject `json:"objects"`
	}

	buf := &bytes.Buffer{}
	tee := io.TeeReader(r.Body, buf)
	var objs batchReq
	err := json.NewDecoder(tee).Decode(&objs)
	io.Copy(ioutil.Discard, r.Body)
	r.Body.Close()

	log.Println("REQUEST")
	log.Println(buf.String())

	if err != nil {
		log.Fatal(err)
	}

	res := []lfsObject{}
	testingChunked := testingChunkedTransferEncoding(r)
	var transferChoice string
	for _, obj := range objs.Objects {
		action := objs.Operation

		o := lfsObject{
			Oid:  obj.Oid,
			Size: obj.Size,
		}

		exists := largeObjects.Has(repo, obj.Oid)
		addAction := true
		if action == "download" {
			if !exists {
				o.Err = &lfsError{Code: 404, Message: fmt.Sprintf("Object %v does not exist", obj.Oid)}
				addAction = false
			}
		} else {
			if exists {
				// not an error but don't add an action
				addAction = false
			}
		}

		switch oidHandlers[obj.Oid] {
		case "status-batch-403":
			o.Err = &lfsError{Code: 403, Message: "welp"}
		case "status-batch-404":
			o.Err = &lfsError{Code: 404, Message: "welp"}
		case "status-batch-410":
			o.Err = &lfsError{Code: 410, Message: "welp"}
		case "status-batch-422":
			o.Err = &lfsError{Code: 422, Message: "welp"}
		case "status-batch-500":
			o.Err = &lfsError{Code: 500, Message: "welp"}
		default: // regular 200 response
			if addAction {
				o.Actions = map[string]lfsLink{
					action: lfsLink{
						Href:   lfsUrl(repo, obj.Oid),
						Header: map[string]string{},
					},
				}
			}
		}

		if testingChunked {
			o.Actions[action].Header["Transfer-Encoding"] = "chunked"
		}

		res = append(res, o)
	}

	ores := batchResp{Transfer: transferChoice, Objects: res}

	by, err := json.Marshal(ores)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("RESPONSE: 200")
	log.Println(string(by))

	w.WriteHeader(200)
	w.Write(by)
}

// Persistent state across requests
var batchResumeFailFallbackStorageAttempts = 0

// handles any /storage/{oid} requests
func storageHandler(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("r")
	parts := strings.Split(r.URL.Path, "/")
	oid := parts[len(parts)-1]
	if missingRequiredCreds(w, r, repo) {
		return
	}

	log.Printf("storage %s %s repo: %s\n", r.Method, oid, repo)
	switch r.Method {
	case "PUT":
		switch oidHandlers[oid] {
		case "status-storage-403":
			w.WriteHeader(403)
			return
		case "status-storage-404":
			w.WriteHeader(404)
			return
		case "status-storage-410":
			w.WriteHeader(410)
			return
		case "status-storage-422":
			w.WriteHeader(422)
			return
		case "status-storage-500":
			w.WriteHeader(500)
			return
		}

		if testingChunkedTransferEncoding(r) {
			valid := false
			for _, value := range r.TransferEncoding {
				if value == "chunked" {
					valid = true
					break
				}
			}
			if !valid {
				log.Fatal("Chunked transfer encoding expected")
			}
		}

		hash := sha256.New()
		buf := &bytes.Buffer{}
		io.Copy(io.MultiWriter(hash, buf), r.Body)
		oid := hex.EncodeToString(hash.Sum(nil))
		if !strings.HasSuffix(r.URL.Path, "/"+oid) {
			w.WriteHeader(403)
			return
		}

		largeObjects.Set(repo, oid, buf.Bytes())

	case "GET":
		parts := strings.Split(r.URL.Path, "/")
		oid := parts[len(parts)-1]
		statusCode := 200
		byteLimit := 0
		resumeAt := int64(0)

		if by, ok := largeObjects.Get(repo, oid); ok {
			if len(by) == len("status-batch-resume-206") && string(by) == "status-batch-resume-206" {
				// Resume if header includes range, otherwise deliberately interrupt
				if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
					regex := regexp.MustCompile(`bytes=(\d+)\-.*`)
					match := regex.FindStringSubmatch(rangeHdr)
					if match != nil && len(match) > 1 {
						statusCode = 206
						resumeAt, _ = strconv.ParseInt(match[1], 10, 32)
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", resumeAt, len(by), resumeAt-int64(len(by))))
					}
				} else {
					byteLimit = 10
				}
			} else if len(by) == len("batch-resume-fail-fallback") && string(by) == "batch-resume-fail-fallback" {
				// Fail any Range: request even though we said we supported it
				// To make sure client can fall back
				if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
					w.WriteHeader(416)
					return
				}
				if batchResumeFailFallbackStorageAttempts == 0 {
					// Truncate output on FIRST attempt to cause resume
					// Second attempt (without range header) is fallback, complete successfully
					byteLimit = 8
					batchResumeFailFallbackStorageAttempts++
				}
			}
			w.WriteHeader(statusCode)
			if byteLimit > 0 {
				w.Write(by[0:byteLimit])
			} else if resumeAt > 0 {
				w.Write(by[resumeAt:])
			} else {
				w.Write(by)
			}
			return
		}

		w.WriteHeader(404)
	default:
		w.WriteHeader(405)
	}
}

func gitHandler(w http.ResponseWriter, r *http.Request) {
	defer func() {
		io.Copy(ioutil.Discard, r.Body)
		r.Body.Close()
	}()

	cmd := exec.Command("git", "http-backend")
	cmd.Env = []string{
		fmt.Sprintf("GIT_PROJECT_ROOT=%s", repoDir),
		fmt.Sprintf("GIT_HTTP_EXPORT_ALL="),
		fmt.Sprintf("PATH_INFO=%s", r.URL.Path),
		fmt.Sprintf("QUERY_STRING=%s", r.URL.RawQuery),
		fmt.Sprintf("REQUEST_METHOD=%s", r.Method),
		fmt.Sprintf("CONTENT_TYPE=%s", r.Header.Get("Content-Type")),
	}

	buffer := &bytes.Buffer{}
	cmd.Stdin = r.Body
	cmd.Stdout = buffer
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}

	text := textproto.NewReader(bufio.NewReader(buffer))

	code, _, _ := text.ReadCodeLine(-1)

	if code != 0 {
		w.WriteHeader(code)
	}

	headers, _ := text.ReadMIMEHeader()
	head := w.Header()
	for key, values := range headers {
		for _, value := range values {
			head.Add(key, value)
		}
	}

	io.Copy(w, text.R)
}

func redirect307Handler(w http.ResponseWriter, r *http.Request) {
	// Send a redirect to info/lfs
	// Make it either absolute or relative depending on subpath
	parts := strings.Split(r.URL.Path, "/")
	// first element is always blank since rooted
	var redirectTo string
	if parts[2] == "rel" {
		redirectTo = "/" + strings.Join(parts[3:], "/")
	} else if parts[2] == "abs" {
		redirectTo = server.URL + "/" + strings.Join(parts[3:], "/")
	} else {
		log.Fatal(fmt.Errorf("Invalid URL for redirect: %v", r.URL))
		w.WriteHeader(404)
		return
	}
	w.Header().Set("Location", redirectTo)
	w.WriteHeader(307)
}

type Committer struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Lock struct {
	Id         string    `json:"id"`
	Path       string    `json:"path"`
	Committer  Committer `json:"committer"`
	CommitSHA  string    `json:"commit_sha"`
	LockedAt   time.Time `json:"locked_at"`
	UnlockedAt time.Time `json:"unlocked_at,omitempty"`
}

type LockRequest struct {
	Path               string    `json:"path"`
	LatestRemoteCommit string    `json:"latest_remote_commit"`
	Committer          Committer `json:"committer"`
}

type LockResponse struct {
	Lock         *Lock  `json:"lock"`
	CommitNeeded string `json:"commit_needed,omitempty"`
	Err          string `json:"error,omitempty"`
}

type UnlockRequest struct {
	Id    string `json:"id"`
	Force bool   `json:"force"`
}

type UnlockResponse struct {
	Lock *Lock  `json:"lock"`
	Err  string `json:"error,omitempty"`
}

type LockList struct {
	Locks      []Lock `json:"locks"`
	NextCursor string `json:"next_cursor,omitempty"`
	Err        string `json:"error,omitempty"`
}

var (
	lmu   sync.RWMutex
	locks = []Lock{}
)

func addLocks(l ...Lock) {
	lmu.Lock()
	defer lmu.Unlock()

	locks = append(locks, l...)

	sort.Sort(LocksByCreatedAt(locks))
}

func getLocks() []Lock {
	lmu.RLock()
	defer lmu.RUnlock()

	return locks
}

type LocksByCreatedAt []Lock

func (c LocksByCreatedAt) Len() int           { return len(c) }
func (c LocksByCreatedAt) Less(i, j int) bool { return c[i].LockedAt.Before(c[j].LockedAt) }
func (c LocksByCreatedAt) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }

var lockRe = regexp.MustCompile(`/locks/?$`)

func locksHandler(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(r.Body)
	enc := json.NewEncoder(w)

	switch r.Method {
	case "GET":
		if !lockRe.MatchString(r.URL.Path) {
			http.NotFound(w, r)
		} else {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "could not parse form values", http.StatusInternalServerError)
				return
			}

			ll := &LockList{}
			locks := getLocks()

			if cursor := r.FormValue("cursor"); cursor != "" {
				lastSeen := -1
				for i, l := range locks {
					if l.Id == cursor {
						lastSeen = i
						break
					}
				}

				if lastSeen > -1 {
					locks = locks[lastSeen:]
				} else {
					enc.Encode(&LockList{
						Err: fmt.Sprintf("cursor (%s) not found", cursor),
					})
				}
			}

			if path := r.FormValue("path"); path != "" {
				var filtered []Lock
				for _, l := range locks {
					if l.Path == path {
						filtered = append(filtered, l)
					}
				}

				locks = filtered
			}

			if limit := r.FormValue("limit"); limit != "" {
				size, err := strconv.Atoi(r.FormValue("limit"))
				if err != nil {
					enc.Encode(&LockList{
						Err: "unable to parse limit amount",
					})
				} else {
					size = int(math.Min(float64(len(locks)), 3))
					if size < 0 {
						locks = []Lock{}
					} else {
						locks = locks[:size]
						if size+1 < len(locks) {
							ll.NextCursor = locks[size+1].Id
						}
					}

				}
			}

			ll.Locks = locks

			enc.Encode(ll)
		}
	case "POST":
		if strings.HasSuffix(r.URL.Path, "unlock") {
			var unlockRequest UnlockRequest
			if err := dec.Decode(&unlockRequest); err != nil {
				enc.Encode(&UnlockResponse{
					Err: err.Error(),
				})
			}

			lockIndex := -1
			for i, l := range locks {
				if l.Id == unlockRequest.Id {
					lockIndex = i
					break
				}
			}

			if lockIndex > -1 {
				enc.Encode(&UnlockResponse{
					Lock: &locks[lockIndex],
				})

				locks = append(locks[:lockIndex], locks[lockIndex+1:]...)
			} else {
				enc.Encode(&UnlockResponse{
					Err: "unable to find lock",
				})
			}
		} else {
			var lockRequest LockRequest
			if err := dec.Decode(&lockRequest); err != nil {
				enc.Encode(&LockResponse{
					Err: err.Error(),
				})
			}

			for _, l := range getLocks() {
				if l.Path == lockRequest.Path {
					enc.Encode(&LockResponse{
						Err: "lock already created",
					})
					return
				}
			}

			var id [20]byte
			rand.Read(id[:])

			lock := &Lock{
				Id:        fmt.Sprintf("%x", id[:]),
				Path:      lockRequest.Path,
				Committer: lockRequest.Committer,
				CommitSHA: lockRequest.LatestRemoteCommit,
				LockedAt:  time.Now(),
			}

			addLocks(*lock)

			// TODO(taylor): commit_needed case
			// TODO(taylor): err case

			enc.Encode(&LockResponse{
				Lock: lock,
			})
		}
	default:
		http.NotFound(w, r)
	}
}

func missingRequiredCreds(w http.ResponseWriter, r *http.Request, repo string) bool {
	if repo != "requirecreds" {
		return false
	}

	auth := r.Header.Get("Authorization")
	user, pass, err := extractAuth(auth)
	if err != nil {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"` + err.Error() + `"}`))
		return true
	}

	if user != "requirecreds" || pass != "pass" {
		errmsg := fmt.Sprintf("Got: '%s' => '%s' : '%s'", auth, user, pass)
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"` + errmsg + `"}`))
		return true
	}

	return false
}

func testingChunkedTransferEncoding(r *http.Request) bool {
	return strings.HasPrefix(r.URL.String(), "/test-chunked-transfer-encoding")
}

var lfsUrlRE = regexp.MustCompile(`\A/?([^/]+)/info/lfs`)

func repoFromLfsUrl(urlpath string) (string, error) {
	matches := lfsUrlRE.FindStringSubmatch(urlpath)
	if len(matches) != 2 {
		return "", fmt.Errorf("LFS url '%s' does not match %v", urlpath, lfsUrlRE)
	}

	repo := matches[1]
	if strings.HasSuffix(repo, ".git") {
		return repo[0 : len(repo)-4], nil
	}
	return repo, nil
}

type lfsStorage struct {
	objects map[string]map[string][]byte
	mutex   *sync.Mutex
}

func (s *lfsStorage) Get(repo, oid string) ([]byte, bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	repoObjects, ok := s.objects[repo]
	if !ok {
		return nil, ok
	}

	by, ok := repoObjects[oid]
	return by, ok
}

func (s *lfsStorage) Has(repo, oid string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	repoObjects, ok := s.objects[repo]
	if !ok {
		return false
	}

	_, ok = repoObjects[oid]
	return ok
}

func (s *lfsStorage) Set(repo, oid string, by []byte) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	repoObjects, ok := s.objects[repo]
	if !ok {
		repoObjects = make(map[string][]byte)
		s.objects[repo] = repoObjects
	}
	repoObjects[oid] = by
}

func (s *lfsStorage) Delete(repo, oid string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	repoObjects, ok := s.objects[repo]
	if ok {
		delete(repoObjects, oid)
	}
}

func newLfsStorage() *lfsStorage {
	return &lfsStorage{
		objects: make(map[string]map[string][]byte),
		mutex:   &sync.Mutex{},
	}
}

func extractAuth(auth string) (string, string, error) {
	if strings.HasPrefix(auth, "Basic ") {
		decodeBy, err := base64.StdEncoding.DecodeString(auth[6:len(auth)])
		decoded := string(decodeBy)

		if err != nil {
			return "", "", err
		}

		parts := strings.SplitN(decoded, ":", 2)
		if len(parts) == 2 {
			return parts[0], parts[1], nil
		}
		return "", "", nil
	}

	return "", "", nil
}

func skipIfBadAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		w.WriteHeader(401)
		return true
	}

	user, pass, err := extractAuth(auth)
	if err != nil {
		w.WriteHeader(403)
		log.Printf("Error decoding auth: %s\n", err)
		return true
	}

	switch user {
	case "user":
		if pass == "pass" {
			return false
		}
	case "netrcuser", "requirecreds":
		return false
	case "path":
		if strings.HasPrefix(r.URL.Path, "/"+pass) {
			return false
		}
		log.Printf("auth attempt against: %q", r.URL.Path)
	}

	w.WriteHeader(403)
	log.Printf("Bad auth: %q\n", auth)
	return true
}

func init() {
	oidHandlers = make(map[string]string)
	for _, content := range contentHandlers {
		h := sha256.New()
		h.Write([]byte(content))
		oidHandlers[hex.EncodeToString(h.Sum(nil))] = content
	}
}
