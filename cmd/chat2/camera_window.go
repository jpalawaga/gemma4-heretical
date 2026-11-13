package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const maxBrowserPhotoBytes = 24 << 20

type browserCaptureResult struct {
	data      []byte
	cancelled bool
}

func capturePhotoWindow(
	ctx context.Context,
	directory string,
	vault *captureVault,
	opener func(string) error,
) (string, bool, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", false, fmt.Errorf("start local camera page: %w", err)
	}

	tokenBytes := make([]byte, 18)
	if _, err := rand.Read(tokenBytes); err != nil {
		listener.Close()
		return "", false, fmt.Errorf("create camera session token: %w", err)
	}
	basePath := "/" + hex.EncodeToString(tokenBytes) + "/"
	result := make(chan browserCaptureResult, 1)
	var resolved atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc(basePath, func(w http.ResponseWriter, r *http.Request) {
		cameraSecurityHeaders(w)
		if r.Method != http.MethodGet || r.URL.Path != basePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, cameraWindowHTML)
	})
	mux.HandleFunc(basePath+"capture", func(w http.ResponseWriter, r *http.Request) {
		cameraSecurityHeaders(w)
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "image/jpeg" {
			http.Error(w, "expected a JPEG photo", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBrowserPhotoBytes)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "photo is too large", http.StatusRequestEntityTooLarge)
			return
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 16384 || config.Height > 16384 {
			http.Error(w, "invalid JPEG photo", http.StatusBadRequest)
			return
		}
		if !resolved.CompareAndSwap(false, true) {
			http.Error(w, "camera session is already complete", http.StatusConflict)
			return
		}
		result <- browserCaptureResult{data: data}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc(basePath+"cancel", func(w http.ResponseWriter, r *http.Request) {
		cameraSecurityHeaders(w)
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if resolved.CompareAndSwap(false, true) {
			result <- browserCaptureResult{cancelled: true}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(basePath+"alive", func(w http.ResponseWriter, r *http.Request) {
		cameraSecurityHeaders(w)
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErrors <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	url := "http://" + listener.Addr().String() + basePath
	if err := opener(url); err != nil {
		return "", false, fmt.Errorf("open camera window: %w", err)
	}

	select {
	case <-ctx.Done():
		return "", true, nil
	case err := <-serverErrors:
		return "", false, fmt.Errorf("camera page stopped: %w", err)
	case capture := <-result:
		if capture.cancelled {
			return "", true, nil
		}
		defer clear(capture.data)
		if vault == nil {
			return "", false, errors.New("encrypted capture vault is unavailable")
		}
		path, err := vault.SealJPEG(directory, capture.data)
		if err != nil {
			return "", false, err
		}
		select {
		case <-ctx.Done():
			if err := removeCaptureFile(path); err != nil {
				return "", false, err
			}
			return "", true, nil
		default:
		}
		return path, false, nil
	}
}

func cameraSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src blob: data:; media-src blob:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "camera=(self)")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func openCameraWindow(url string) error {
	var commands [][]string
	switch runtime.GOOS {
	case "darwin":
		if defaultBrowserBundleID() == "org.mozilla.firefox" {
			if err := openFirefoxWindow(url); err == nil {
				return nil
			}
		}
		// Passing a URL together with `open -n` starts some browsers, notably
		// Firefox, with a blank window. Plain `open` preserves LaunchServices'
		// URL handoff and is the safest fallback for unknown browsers.
		commands = [][]string{{"open", url}}
	case "windows":
		commands = [][]string{{"rundll32", "url.dll,FileProtocolHandler", url}}
	default:
		commands = [][]string{{"xdg-open", url}}
	}
	var lastErr error
	for _, command := range commands {
		if err := exec.Command(command[0], command[1:]...).Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func defaultBrowserBundleID() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	output, err := exec.Command(
		"defaults",
		"read",
		"com.apple.LaunchServices/com.apple.launchservices.secure",
		"LSHandlers",
	).Output()
	if err != nil {
		return ""
	}
	return browserBundleIDFromLaunchServices(string(output), "http")
}

func browserBundleIDFromLaunchServices(content, scheme string) string {
	for _, block := range strings.Split(content, "},") {
		if !strings.Contains(block, "LSHandlerURLScheme = "+scheme+";") &&
			!strings.Contains(block, `LSHandlerURLScheme = "`+scheme+`";`) {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "LSHandlerRoleAll =") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, "LSHandlerRoleAll ="))
			return strings.Trim(strings.TrimSuffix(value, ";"), `"`)
		}
	}
	return ""
}

func openFirefoxWindow(url string) error {
	for _, path := range []string{
		"/Applications/Firefox.app/Contents/MacOS/firefox",
		filepath.Join(os.Getenv("HOME"), "Applications/Firefox.app/Contents/MacOS/firefox"),
	} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		command := exec.Command(path, "-new-window", url)
		if err := command.Start(); err != nil {
			continue
		}
		return command.Process.Release()
	}
	return exec.Command(
		"open", "-n", "-b", "org.mozilla.firefox", "--args", "-new-window", url,
	).Run()
}

const cameraWindowHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>chat2 camera</title>
<style>
:root { color-scheme: dark; font-family: ui-sans-serif, system-ui, sans-serif; background:#0f111a; color:#e8eaf2 }
* { box-sizing:border-box }
body { margin:0; min-height:100vh; display:grid; place-items:center; background:radial-gradient(circle at top,#25203d,#0f111a 55%) }
main { width:min(1100px,96vw); padding:24px }
header { display:flex; gap:18px; align-items:end; justify-content:space-between; margin-bottom:14px }
h1 { margin:0; font-size:clamp(22px,4vw,38px); color:#c4b5fd }
p { margin:5px 0 0; color:#9ca3af }
select,button { font:inherit; color:#f5f3ff; border:1px solid #4c4567; background:#242033; border-radius:10px; padding:10px 15px }
button { cursor:pointer; font-weight:700 }
button:hover:not(:disabled) { border-color:#67e8f9; transform:translateY(-1px) }
button:disabled { opacity:.45; cursor:not-allowed }
.primary { color:#10121a; background:#67e8f9; border-color:#67e8f9 }
.danger { color:#fecdd3; border-color:#9f4055 }
.frame { position:relative; overflow:hidden; min-height:300px; border:1px solid #49405f; border-radius:18px; background:#050609; box-shadow:0 25px 80px #0008 }
video,img { display:block; width:100%; max-height:72vh; object-fit:contain; background:#050609 }
img[hidden],video[hidden] { display:none }
.badge { position:absolute; top:14px; left:14px; padding:7px 10px; border-radius:999px; background:#10121acc; color:#86efac; font-size:13px }
.countdown { position:absolute; inset:0; display:grid; place-items:center; color:#fff; background:#05060966; font-size:clamp(92px,22vw,240px); font-weight:900; line-height:1; text-shadow:0 8px 40px #000 }
.countdown[hidden] { display:none }
.actions { display:flex; flex-wrap:wrap; justify-content:center; gap:10px; margin-top:16px }
#message { min-height:24px; text-align:center; color:#f9a8d4; margin-top:12px }
#done { text-align:center; padding:80px 20px }
@media (max-width:650px) { header { align-items:start; flex-direction:column } main { padding:12px } }
</style>
</head>
<body>
<main>
  <header><div><h1>chat2 camera</h1><p>Preview at camera resolution. Nothing is shared until you choose Use Photo.</p></div><select id="device" aria-label="Camera"><option>Default camera</option></select></header>
  <section id="stage">
    <div class="frame"><video id="video" autoplay playsinline></video><img id="still" alt="Captured preview" hidden><span class="badge" id="badge">LIVE</span><div id="countdown" class="countdown" aria-live="assertive" hidden></div></div>
    <div class="actions"><button id="capture" class="primary" disabled>Capture</button><button id="timer" disabled>3s Timer</button><button id="retake" hidden>Retake</button><button id="use" class="primary" hidden>Use Photo</button><button id="cancel" class="danger">Cancel</button></div>
    <div id="message" role="status">Requesting camera permission…</div>
  </section>
  <section id="done" hidden><h1>Photo returned to chat2</h1><p>The photo was accepted. You can close this window.</p><p><button onclick="window.close()">Close Window</button></p></section>
</main>
<canvas id="canvas" hidden></canvas>
<script>
const get = id => document.getElementById(id);
const video=get('video'), still=get('still'), canvas=get('canvas'), capture=get('capture'), timer=get('timer'), retake=get('retake'), use=get('use'), cancel=get('cancel'), device=get('device'), message=get('message'), badge=get('badge'), countdown=get('countdown');
let stream, photo, completed=false, objectURL, countdownGeneration=0;
const sleep=milliseconds=>new Promise(resolve=>setTimeout(resolve,milliseconds));
function stopCamera(){ if(stream) stream.getTracks().forEach(track=>track.stop()); stream=null }
async function startCamera(deviceId=''){
  cancelCountdown(); stopCamera(); capture.disabled=true; timer.disabled=true; message.textContent='Opening camera…';
  const videoConstraint=deviceId?{deviceId:{exact:deviceId},width:{ideal:1920},height:{ideal:1080}}:{width:{ideal:1920},height:{ideal:1080}};
  stream=await navigator.mediaDevices.getUserMedia({video:videoConstraint,audio:false});
  video.srcObject=stream; await video.play(); capture.disabled=false; timer.disabled=false;
  message.textContent=video.videoWidth+' × '+video.videoHeight+' live preview';
  const devices=(await navigator.mediaDevices.enumerateDevices()).filter(item=>item.kind==='videoinput');
  const active=stream.getVideoTracks()[0].getSettings().deviceId;
  device.replaceChildren(...devices.map((item,index)=>{const option=document.createElement('option');option.value=item.deviceId;option.textContent=item.label||('Camera '+(index+1));option.selected=item.deviceId===active;return option}));
}
function captureFrame(){
  if(completed||photo||!stream)return;
  canvas.width=video.videoWidth; canvas.height=video.videoHeight;
  canvas.getContext('2d').drawImage(video,0,0,canvas.width,canvas.height);
  canvas.toBlob(blob=>{
    photo=blob; if(objectURL) URL.revokeObjectURL(objectURL); objectURL=URL.createObjectURL(blob); still.src=objectURL;
    video.hidden=true; still.hidden=false; badge.textContent='CAPTURED'; badge.style.color='#f9a8d4';
    capture.hidden=true; timer.hidden=true; retake.hidden=false; use.hidden=false; device.disabled=false; message.textContent='Review the full-resolution frame before using it.';
  },'image/jpeg',0.94);
}
function cancelCountdown(){
  countdownGeneration++; countdown.hidden=true; countdown.textContent=''; device.disabled=false;
  if(stream&&!photo&&!completed){capture.disabled=false;timer.disabled=false}
}
async function timedCapture(){
  if(completed||photo||!stream)return;
  const generation=++countdownGeneration;
  capture.disabled=true; timer.disabled=true; device.disabled=true; countdown.hidden=false;
  for(let remaining=3;remaining>=1;remaining--){
    if(generation!==countdownGeneration||completed)return;
    countdown.textContent=remaining; message.textContent='Capturing in '+remaining+'…';
    await sleep(1000);
  }
  if(generation!==countdownGeneration||completed)return;
  countdown.hidden=true; countdown.textContent=''; device.disabled=false; captureFrame();
}
capture.onclick=captureFrame; timer.onclick=timedCapture;
retake.onclick=()=>{ cancelCountdown(); photo=null; still.hidden=true; video.hidden=false; badge.textContent='LIVE'; badge.style.color='#86efac';capture.hidden=false;timer.hidden=false;timer.disabled=false;retake.hidden=true;use.hidden=true;message.textContent=video.videoWidth+' × '+video.videoHeight+' live preview' };
use.onclick=async()=>{
  if(!photo)return; use.disabled=true; message.textContent='Returning photo to chat2…';
  try { const response=await fetch('capture',{method:'POST',headers:{'Content-Type':'image/jpeg'},body:photo}); if(!response.ok)throw new Error(await response.text()); completed=true; cancelCountdown(); stopCamera(); get('stage').hidden=true; get('done').hidden=false; }
  catch(error){ use.disabled=false; message.textContent='Could not return photo: '+error.message }
};
async function cancelSession(){ if(completed)return; completed=true; cancelCountdown(); stopCamera(); try{await fetch('cancel',{method:'POST',keepalive:true})}catch{} get('stage').hidden=true; get('done').hidden=false; get('done').querySelector('h1').textContent='Capture cancelled'; get('done').querySelector('p').textContent='No photo was kept or returned to chat2.' }
cancel.onclick=cancelSession; device.onchange=()=>startCamera(device.value).catch(showError);
function showError(error){ capture.disabled=true; timer.disabled=true; message.textContent='Camera unavailable: '+error.message }
window.addEventListener('keydown',event=>{if(event.key==='Escape')cancelSession()});
window.addEventListener('beforeunload',()=>{stopCamera();if(!completed)navigator.sendBeacon('cancel')});
setInterval(async()=>{if(completed)return;try{const response=await fetch('alive',{cache:'no-store'});if(!response.ok)throw new Error()}catch{completed=true;stopCamera();get('stage').hidden=true;get('done').hidden=false;get('done').querySelector('h1').textContent='Camera session closed';get('done').querySelector('p').textContent='The capture was cancelled from chat2. No photo was kept.'}},700);
startCamera().catch(showError);
</script>
</body>
</html>`
