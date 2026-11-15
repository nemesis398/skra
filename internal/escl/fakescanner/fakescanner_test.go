// SPDX-License-Identifier: AGPL-3.0-or-later

package fakescanner_test

import (
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nemesis398/skra/internal/escl/fakescanner"
)

func start(t *testing.T) (*fakescanner.Scanner, *httptest.Server) {
	t.Helper()
	s := fakescanner.New(fakescanner.Options{})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func settings(source string, duplex bool) string {
	d := "false"
	if duplex {
		d = "true"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScanSettings xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
<pwg:Version>2.0</pwg:Version>
<pwg:ScanRegions><pwg:ScanRegion>
<pwg:ContentRegionUnits>escl:ThreeHundredthsOfInches</pwg:ContentRegionUnits>
<pwg:XOffset>0</pwg:XOffset><pwg:YOffset>0</pwg:YOffset>
<pwg:Width>2480</pwg:Width><pwg:Height>3508</pwg:Height>
</pwg:ScanRegion></pwg:ScanRegions>
<pwg:InputSource>` + source + `</pwg:InputSource>
<scan:Duplex>` + d + `</scan:Duplex>
<scan:ColorMode>Grayscale8</scan:ColorMode>
<scan:XResolution>300</scan:XResolution>
<scan:YResolution>300</scan:YResolution>
<scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt>
<scan:SomethingNew>ignored</scan:SomethingNew>
</scan:ScanSettings>`
}

func post(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/eSCL/ScanJobs", "text/xml", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// nextPage fetches the next document and, for a 200, returns the decoded
// page. Any other status comes back with a nil image.
func nextPage(t *testing.T, srv *httptest.Server, loc string) (int, image.Image) {
	t.Helper()
	resp, body := get(t, srv.URL+loc+"/NextDocument")
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
	img, err := jpeg.Decode(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("page is not a JPEG: %v", err)
	}
	return resp.StatusCode, img
}

// bandTop is the first row that is dark, which tells the pages apart.
func bandTop(t *testing.T, img image.Image) int {
	t.Helper()
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if color.GrayModel.Convert(img.At(b.Min.X+b.Dx()/2, y)).(color.Gray).Y < 128 {
			return y
		}
	}
	t.Fatal("page has no dark band")
	return 0
}

// pages reads every page of a job and returns the band position of each.
func pages(t *testing.T, srv *httptest.Server, loc string) []int {
	t.Helper()
	var tops []int
	for {
		status, img := nextPage(t, srv, loc)
		if status == http.StatusNotFound {
			return tops
		}
		if status != http.StatusOK {
			t.Fatalf("NextDocument status = %d", status)
		}
		tops = append(tops, bandTop(t, img))
	}
}

func distinct(tops []int) bool {
	seen := map[int]bool{}
	for _, top := range tops {
		if seen[top] {
			return false
		}
		seen[top] = true
	}
	return true
}

func TestCapabilitiesLookLikeTheBrother(t *testing.T) {
	_, srv := start(t)
	resp, body := get(t, srv.URL+"/eSCL/ScannerCapabilities")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	doc := string(body)
	for _, want := range []string{
		"<scan:Platen>", "<scan:Adf>", "<scan:AdfDuplexInputCaps>",
		"BlackAndWhite1", "Grayscale8", "RGB24",
		"<scan:XResolution>100<", "<scan:XResolution>200<",
		"<scan:XResolution>300<", "<scan:XResolution>600<",
		"image/jpeg", "application/pdf",
		"skrá fake scanner",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("capabilities lack %q", want)
		}
	}
	if strings.Contains(doc, "E84719D6H124498") {
		t.Error("capabilities carry the real serial number")
	}
}

func TestGlassGivesOnePage(t *testing.T) {
	s, srv := start(t)
	resp := post(t, srv, settings("Platen", false))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/eSCL/ScanJobs/") {
		t.Fatalf("Location = %q", loc)
	}
	status, img := nextPage(t, srv, loc)
	if status != http.StatusOK {
		t.Fatalf("first NextDocument = %d", status)
	}
	if b := img.Bounds(); b.Dx() != 2480 || b.Dy() != 3508 {
		t.Errorf("page is %dx%d, want 2480x3508", b.Dx(), b.Dy())
	}
	if status, _ := nextPage(t, srv, loc); status != http.StatusNotFound {
		t.Errorf("second NextDocument = %d, want 404", status)
	}
	last := s.LastSettings()
	want := fakescanner.Settings{
		InputSource: "Platen", ColorMode: "Grayscale8",
		XResolution: 300, YResolution: 300, DocumentFormatExt: "image/jpeg",
		Width: 2480, Height: 3508,
		ContentRegionUnits: "escl:ThreeHundredthsOfInches",
	}
	if last != want {
		t.Errorf("LastSettings = %+v, want %+v", last, want)
	}
}

func TestResolutionScalesThePage(t *testing.T) {
	_, srv := start(t)
	body := strings.Replace(settings("Platen", false), "<scan:XResolution>300<", "<scan:XResolution>100<", 1)
	body = strings.Replace(body, "<scan:YResolution>300<", "<scan:YResolution>100<", 1)
	resp := post(t, srv, body)
	_, img := nextPage(t, srv, resp.Header.Get("Location"))
	if b := img.Bounds(); b.Dx() != 827 || b.Dy() != 1169 {
		t.Errorf("page is %dx%d, want 827x1169", b.Dx(), b.Dy())
	}
}

func TestMissingInputSourceMeansPlaten(t *testing.T) {
	s, srv := start(t)
	resp := post(t, srv, `<scan:ScanSettings xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03"></scan:ScanSettings>`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	if got := s.LastSettings().InputSource; got != "Platen" {
		t.Errorf("InputSource = %q, want Platen", got)
	}
	if got := len(pages(t, srv, resp.Header.Get("Location"))); got != 1 {
		t.Errorf("got %d pages, want 1", got)
	}
}

func TestBrokenSettingsAreRefused(t *testing.T) {
	_, srv := start(t)
	if resp := post(t, srv, "<not xml"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFeederGivesItsPagesAndBothSides(t *testing.T) {
	_, srv := start(t)
	resp := post(t, srv, settings("Feeder", false))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	tops := pages(t, srv, resp.Header.Get("Location"))
	if len(tops) != 3 || !distinct(tops) {
		t.Errorf("simplex bands = %v, want 3 distinct pages", tops)
	}

	resp = post(t, srv, settings("Feeder", true))
	tops = pages(t, srv, resp.Header.Get("Location"))
	if len(tops) != 6 || !distinct(tops) {
		t.Errorf("duplex bands = %v, want 6 distinct pages", tops)
	}
}

func TestAdfPagesOption(t *testing.T) {
	s := fakescanner.New(fakescanner.Options{AdfPages: 2})
	srv := httptest.NewServer(s)
	defer srv.Close()
	resp := post(t, srv, settings("Feeder", false))
	if got := len(pages(t, srv, resp.Header.Get("Location"))); got != 2 {
		t.Errorf("got %d pages, want 2", got)
	}
}

func scannerStatus(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, body := get(t, srv.URL+"/eSCL/ScannerStatus")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ScannerStatus = %d", resp.StatusCode)
	}
	return string(body)
}

func TestFeederEmptyBusyAndJam(t *testing.T) {
	s, srv := start(t)
	if !strings.Contains(scannerStatus(t, srv), "ScannerAdfLoaded") {
		t.Error("a fresh scanner does not report ScannerAdfLoaded")
	}

	s.SetState("empty", false)
	if resp := post(t, srv, settings("Feeder", false)); resp.StatusCode != http.StatusConflict {
		t.Errorf("empty feeder: POST = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(scannerStatus(t, srv), "ScannerAdfEmpty") {
		t.Error("status lacks ScannerAdfEmpty")
	}
	if resp := post(t, srv, settings("Platen", false)); resp.StatusCode != http.StatusCreated {
		t.Errorf("empty feeder: glass POST = %d, want 201", resp.StatusCode)
	}

	s.SetState("loaded", true)
	if resp := post(t, srv, settings("Platen", false)); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("busy: POST = %d, want 503", resp.StatusCode)
	}

	s.SetState("loaded", false)
	resp := post(t, srv, settings("Feeder", false))
	loc := resp.Header.Get("Location")
	s.SetState("jam", false)
	if status, _ := nextPage(t, srv, loc); status != http.StatusConflict {
		t.Errorf("jam: NextDocument = %d, want 409", status)
	}
	if !strings.Contains(scannerStatus(t, srv), "ScannerAdfJam") {
		t.Error("status lacks ScannerAdfJam")
	}
}

func TestStateEndpoint(t *testing.T) {
	_, srv := start(t)
	setState := func(form url.Values) int {
		resp, err := http.PostForm(srv.URL+"/_fake/state", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := setState(url.Values{"adf": {"empty"}}); code != http.StatusNoContent {
		t.Fatalf("state = %d, want 204", code)
	}
	if !strings.Contains(scannerStatus(t, srv), "ScannerAdfEmpty") {
		t.Error("adf=empty did not take")
	}
	if code := setState(url.Values{"busy": {"true"}}); code != http.StatusNoContent {
		t.Fatalf("state = %d, want 204", code)
	}
	if resp := post(t, srv, settings("Platen", false)); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("busy: POST = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(scannerStatus(t, srv), "ScannerAdfEmpty") {
		t.Error("setting busy alone changed the feeder")
	}
	if code := setState(url.Values{"adf": {"stuck"}}); code != http.StatusBadRequest {
		t.Errorf("bad adf = %d, want 400", code)
	}
	if code := setState(url.Values{"busy": {"maybe"}}); code != http.StatusBadRequest {
		t.Errorf("bad busy = %d, want 400", code)
	}
}

func TestDeleteEndsTheJob(t *testing.T) {
	_, srv := start(t)
	resp := post(t, srv, settings("Feeder", false))
	loc := resp.Header.Get("Location")
	req, err := http.NewRequest(http.MethodDelete, srv.URL+loc, nil)
	if err != nil {
		t.Fatal(err)
	}
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", del.StatusCode)
	}
	if status, _ := nextPage(t, srv, loc); status != http.StatusNotFound {
		t.Errorf("NextDocument after DELETE = %d, want 404", status)
	}
}
