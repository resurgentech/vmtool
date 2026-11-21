package vmtool

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Export volumes: the thick LVs `templates export` writes a template's qcow2
// into (deploy/vmtool-thin, verbs export and send). The helper client and the
// pure pieces around it live here; internal/hosts decides when to make, reuse
// and remove them.

// ExportVolPrefix starts the name of every export volume. VM names may not
// start with it, and vmtool's volume listings leave such names out.
const ExportVolPrefix = "vmexp-"

// ExportTTL is how long an export volume lives, counted from its lv_time.
const ExportTTL = 24 * time.Hour

var exportLVRE = regexp.MustCompile(`^vmexp-[0-9a-f]{12}$`)

// ValidExportLVName reports whether s is vmexp- and 12 lowercase hex
// characters, the only shape the helper accepts for an export volume.
func ValidExportLVName(s string) bool { return exportLVRE.MatchString(s) }

// NewExportLVName is a fresh export volume name from 6 random bytes of r (use
// crypto/rand.Reader). The caller checks that no LV of the VG has it.
func NewExportLVName(r io.Reader) (string, error) {
	var b [6]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("export volume name: %w", err)
	}
	return ExportVolPrefix + hex.EncodeToString(b[:]), nil
}

// IsExportVolumeName reports whether a libvirt volume or LV name is one of an
// export volume (the vmexp- prefix).
func IsExportVolumeName(name string) bool { return strings.HasPrefix(name, ExportVolPrefix) }

// isImageVolumeName: a libvirt pool volume that is a cached image, which is a
// .qcow2 name and never an export volume's.
func isImageVolumeName(name string) bool {
	return strings.HasSuffix(name, ".qcow2") && !IsExportVolumeName(name)
}

// FilterImageNames keeps the .qcow2 names of a volume listing that are not
// export volumes: the one rule vmtool's image listings (ListImages,
// ListImagesByPool) apply to what a libvirt pool lists.
func FilterImageNames(names []string) []string {
	var out []string
	for _, n := range names {
		if isImageVolumeName(n) {
			out = append(out, n)
		}
	}
	return out
}

// ThinExport is an export volume as its tags describe it. Bytes is 0 until
// the helper has finished the export (then the qcow2's length); SHA256 is
// empty until the digest is known.
type ThinExport struct {
	Name         string
	VG           string
	TemplateName string // vmtool_name: the template's name
	From         string // vmtool_from: the template's LV
	Bytes        int64
	SHA256       string
	SizeBytes    int64 // the volume's own size
	Created      time.Time
}

// Complete reports whether the export finished (it has vmtool_bytes).
func (e ThinExport) Complete() bool { return e.Bytes > 0 }

// Expires is when the daemon removes the export; zero when its age is unknown.
func (e ThinExport) Expires() time.Time {
	if e.Created.IsZero() {
		return time.Time{}
	}
	return e.Created.Add(ExportTTL)
}

// ExportFromVolume reads an export volume's tags. A volume of another kind
// gives the zero value.
func ExportFromVolume(v ThinVolume) ThinExport {
	if v.Kind != ThinKindExport {
		return ThinExport{}
	}
	e := ThinExport{Name: v.Name, VG: v.VG, TemplateName: tagValue(v.Tags, thinTagNamePrefix),
		From: tagValue(v.Tags, thinTagFromPrefix), SHA256: tagValue(v.Tags, thinTagSHA256Prefix),
		SizeBytes: v.SizeBytes, Created: v.Created}
	if n, err := strconv.ParseInt(tagValue(v.Tags, thinTagBytesPrefix), 10, 64); err == nil && n > 0 {
		e.Bytes = n
	}
	if !sha256RE.MatchString(e.SHA256) {
		e.SHA256 = ""
	}
	return e
}

// ExportTags is the tag list of a new export volume of the template LV from,
// whose name is templateName.
func ExportTags(templateName, from string) []string {
	return []string{ThinTagExport, thinTagNamePrefix + templateName, thinTagFromPrefix + from}
}

// ExportSHATag is the tag text of an export volume's (or a template's) digest.
func ExportSHATag(sha string) string { return thinTagSHA256Prefix + sha }

var progressRE = regexp.MustCompile(`\(([0-9]+(?:\.[0-9]+)?)/100%\)`)

// ParseExportProgress reads the percentage out of qemu-img's -p output, such as
// "    (12.34/100%)" (a chunk may hold several; the last counts). Text without a
// well-formed 0..100 figure gives ok false.
func ParseExportProgress(s string) (pct float64, ok bool) {
	for _, m := range progressRE.FindAllStringSubmatch(s, -1) {
		f, err := strconv.ParseFloat(m[1], 64)
		if err != nil || f < 0 || f > 100 {
			continue
		}
		pct, ok = f, true
	}
	return pct, ok
}

// ExportRoomError is the helper's refusal to start an export: the array's
// volume group does not have the room. It matches ErrThinNoRoom.
type ExportRoomError struct {
	Template string // the template's name, set by the caller that knows it
	VG       string
	Need     int64 // the export volume's size
	Free     int64 // the group's free bytes
	Reserve  int64 // what the thin pool keeps to grow
}

func (e *ExportRoomError) Is(target error) bool { return target == ErrThinNoRoom }

func gbText(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

func (e *ExportRoomError) Error() string {
	avail := max(e.Free-e.Reserve, 0)
	return fmt.Sprintf("export of %s needs %s in volume group %s; %s is free outside the thin pool after keeping %s for it to grow; nothing was written",
		e.Template, gbText(e.Need), e.VG, gbText(avail), gbText(e.Reserve))
}

var noRoomRE = regexp.MustCompile(`no room: need=(\d+) free=(\d+) reserve=(\d+)`)

func parseNoRoom(stderr string) (need, free, reserve int64, ok bool) {
	m := noRoomRE.FindStringSubmatch(stderr)
	if m == nil {
		return 0, 0, 0, false
	}
	var v [3]int64
	for i := range v {
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil {
			return 0, 0, 0, false
		}
		v[i] = n
	}
	return v[0], v[1], v[2], true
}

// helperFailure is the one-line form of a helper's stderr, as run reports it.
func helperFailure(verb, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	msg = strings.TrimPrefix(msg, "vmtool-thin: ")
	msg = strings.Join(strings.Fields(strings.ReplaceAll(msg, "\n", " ; ")), " ")
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("vmtool-thin %s: %s", verb, msg)
}

// scanProgress splits qemu-img's progress output at carriage returns and
// newlines.
func scanProgress(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// ExportThick runs the helper's export: the template LV (a vmtool_template) is
// converted into a new thick volume name in vg, tagged with tags (which must
// include vmtool_export; see ExportTags). on, when not nil, gets the percent
// done as qemu-img reports it. Without room the helper refuses before
// creating anything and the error is an *ExportRoomError (it matches
// ErrThinNoRoom; Template is left for the caller). Any other failure removes
// the volume (the helper does that) and comes back as the helper's line.
func (t *Thin) ExportThick(vg, template, name string, tags []string, on func(pct float64)) error {
	err := firstErr(thinCheckName("volume group", vg), thinCheckName("template", template), thinCheckTags(tags))
	if err == nil && !ValidExportLVName(name) {
		err = fmt.Errorf("invalid export volume name %q", name)
	}
	if err == nil && !hasTag(tags, ThinTagExport) {
		err = errors.New("the tags must include " + ThinTagExport)
	}
	if err != nil {
		return fmt.Errorf("vmtool-thin export: %w", err)
	}
	cmd, err := command("sudo", append([]string{"-n", t.helper(), "export", vg, template, name}, tags...)...)
	if err != nil {
		return fmt.Errorf("vmtool-thin export: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("vmtool-thin export: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("vmtool-thin export: %w", err)
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	sc.Split(scanProgress)
	for sc.Scan() {
		if on == nil {
			continue
		}
		if pct, ok := ParseExportProgress(sc.Text()); ok {
			on(pct)
		}
	}
	_, _ = io.Copy(io.Discard, out) // a line too long for the scanner: keep draining
	if werr := cmd.Wait(); werr != nil {
		if need, free, reserve, ok := parseNoRoom(stderr.String()); ok {
			return &ExportRoomError{VG: vg, Need: need, Free: free, Reserve: reserve}
		}
		return helperFailure("export", stderr.String(), werr)
	}
	return nil
}

// Send starts the helper's send of the export volume name and returns a reader
// of its stdout: exactly bytes bytes (the caller read vmtool_bytes from the
// report; the helper reads the same tag itself and writes that many). A
// stream that ends short, or a helper that exits non-zero, is an error from
// Read, never a plain EOF. Close stops the helper if it is still running and
// waits for it; call it always.
func (t *Thin) Send(vg, name string, bytes int64) (io.ReadCloser, error) {
	err := firstErr(thinCheckName("volume group", vg), thinCheckBytes(bytes))
	if err == nil && !ValidExportLVName(name) {
		err = fmt.Errorf("invalid export volume name %q", name)
	}
	if err != nil {
		return nil, fmt.Errorf("vmtool-thin send: %w", err)
	}
	cmd, err := command("sudo", "-n", t.helper(), "send", vg, name)
	if err != nil {
		return nil, fmt.Errorf("vmtool-thin send: %w", err)
	}
	r := &sendReader{cmd: cmd, want: bytes}
	cmd.Stderr = &r.stderr
	if r.out, err = cmd.StdoutPipe(); err != nil {
		return nil, fmt.Errorf("vmtool-thin send: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vmtool-thin send: %w", err)
	}
	return r, nil
}

type sendReader struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr bytes.Buffer
	want   int64
	got    int64
	err    error // sticky: the end, or what went wrong

	once    sync.Once
	waitErr error
}

func (r *sendReader) wait() error {
	r.once.Do(func() {
		if err := r.cmd.Wait(); err != nil {
			r.waitErr = helperFailure("send", r.stderr.String(), err)
		}
	})
	return r.waitErr
}

func (r *sendReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if rest := r.want - r.got; int64(len(p)) > rest {
		p = p[:rest]
	}
	if r.got >= r.want { // every byte has been read: only the exit status is left
		if err := r.wait(); err != nil {
			r.err = err
		} else {
			r.err = io.EOF
		}
		return 0, r.err
	}
	n, err := r.out.Read(p)
	r.got += int64(n)
	if err == io.EOF {
		if r.got < r.want {
			err = fmt.Errorf("vmtool-thin send: the stream ended after %d of %d bytes", r.got, r.want)
			if werr := r.wait(); werr != nil {
				err = fmt.Errorf("%w (%v)", err, werr)
			}
			r.err = err
			return n, err
		}
		err = nil // the next call reports the end
	}
	if err != nil {
		r.err = err
	}
	return n, err
}

// Close stops the helper if it still runs and waits for it. Its error is that
// of a helper that failed on its own; stopping it early is not one.
func (r *sendReader) Close() error {
	done := r.err != nil
	_ = r.out.Close()
	if !done && r.got < r.want {
		_ = r.cmd.Process.Kill()
		_ = r.wait()
		return nil
	}
	return r.wait()
}
