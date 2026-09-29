#!/usr/bin/env python3
"""Exercise the running Compose stack without printing credentials or tokens."""
import json
import pathlib
import secrets
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid
import zlib

BASE = "http://127.0.0.1:8080"

def api(method, path, token="", body=None, headers=None):
    data = None if body is None else (body if isinstance(body, bytes) else json.dumps(body).encode())
    h = {"Content-Type": "application/json", **(headers or {})}
    if token:
        h["Authorization"] = "Bearer " + token
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, json.loads(raw) if raw and resp.headers.get_content_type() == "application/json" else raw
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        try:
            return exc.code, json.loads(raw)
        except Exception:
            return exc.code, raw

def must(status, expected, action):
    if status != expected:
        raise AssertionError(f"{action}: expected {expected}, got {status}")

def png_image():
    def chunk(kind,payload):
        return struct.pack('!I',len(payload))+kind+payload+struct.pack('!I',zlib.crc32(kind+payload)&0xffffffff)
    pixels=b''.join(b'\0'+b'\x30\x90\xc0'*32 for _ in range(32))
    return b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('!IIBBBBB',32,32,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(pixels))+chunk(b'IEND',b'')

def main():
    password = pathlib.Path("secrets/admin-password.txt").read_text().strip()
    status, admin = api("POST", "/api/v1/auth/login", body={"login": "admin", "password": password})
    must(status, 200, "admin login")
    at = admin["token"]
    suffix = secrets.token_hex(5)
    alice_name, bob_name = "e2ea" + suffix, "e2eb" + suffix
    alice_pass, bob_pass = secrets.token_urlsafe(18), secrets.token_urlsafe(18)
    status, _ = api("POST", "/api/v1/auth/register", body={"login": bob_name, "password": bob_pass})
    must(status, 403, "registration off")
    status, _ = api("POST", "/api/v1/admin/users", at, {"login": alice_name, "password": alice_pass})
    must(status, 201, "admin create user")
    status, _ = api("PATCH", "/api/v1/admin/settings/registration", at, {"enabled": True})
    must(status, 200, "enable registration")
    try:
        status, _ = api("POST", "/api/v1/auth/register", body={"login": bob_name, "password": bob_pass})
        must(status, 201, "self registration")
    finally:
        status, _ = api("PATCH", "/api/v1/admin/settings/registration", at, {"enabled": False})
        must(status, 200, "disable registration")
    status, alice = api("POST", "/api/v1/auth/login", body={"login": alice_name, "password": alice_pass})
    must(status, 200, "alice login")
    status, bob = api("POST", "/api/v1/auth/login", body={"login": bob_name, "password": bob_pass})
    must(status, 200, "bob login")
    token_a, token_b = alice["token"], bob["token"]
    id_b = bob["user"]["id"]
    status, chat = api("POST", "/api/v1/chats/direct", token_a, {"user_id": id_b})
    must(status, 201, "direct chat")
    chat_id = chat["id"]
    status, file = api("POST", f"/api/v1/chats/{chat_id}/files", token_a, b"family private bytes", {"Content-Type": "text/plain", "X-File-Name": "family.txt"})
    must(status, 201, "upload")
    file_id = file["id"]
    path = f"/api/v1/files/{file_id}/content"
    must(api("GET", path, token_b)[0], 404, "recipient before send")
    status, _ = api("POST", f"/api/v1/chats/{chat_id}/messages", token_a, {"client_message_id": str(uuid.uuid4()), "text": "Семейный файл", "attachment_ids": [file_id]})
    must(status, 201, "send message")
    must(api("GET", path, token_b)[0], 200, "recipient file")
    must(api("GET", path, at)[0], 404, "nonmember admin file")
    must(api("GET", path)[0], 401, "anonymous file")
    must(api("GET", path, token_b, headers={"Range": "bytes=0-5"})[0], 206, "authorized range")
    status, history = api("GET", f"/api/v1/chats/{chat_id}/messages", token_b)
    must(status, 200, "history")
    if file_id not in json.dumps(history):
        raise AssertionError("attachment metadata absent from history")
    status, results = api("GET", f"/api/v1/chats/{chat_id}/search?q=%D0%A1%D0%B5%D0%BC%D0%B5%D0%B9%D0%BD%D1%8B%D0%B9", token_b)
    must(status, 200, "history search")
    if not results["messages"]:
        raise AssertionError("search missed saved message")
    for filename,ctype,contents in [('photo.png','image/png',png_image())]:
        status, media=api('POST',f'/api/v1/chats/{chat_id}/files',token_a,contents,{'Content-Type':ctype,'X-File-Name':filename})
        must(status,201,'media upload')
        must(api('POST',f'/api/v1/chats/{chat_id}/messages',token_a,{'client_message_id':str(uuid.uuid4()),'text':filename,'attachment_ids':[media['id']]})[0],201,'media message')
        for _ in range(30):
            status,meta=api('GET',f"/api/v1/files/{media['id']}",token_b)
            must(status,200,'media metadata')
            if meta['preview_state']=='ready':break
            if meta['preview_state']=='failed':raise AssertionError('preview failed')
            time.sleep(.3)
        else:raise AssertionError('preview timed out')
        must(api('GET',f"/api/v1/files/{media['id']}/preview",token_b)[0],200,'recipient preview')
        must(api('GET',f"/api/v1/files/{media['id']}/preview",at)[0],404,'outsider preview')
    with tempfile.NamedTemporaryFile(suffix='.mp4') as video:
        subprocess.run(['ffmpeg','-nostdin','-v','error','-y','-f','lavfi','-i','color=c=blue:s=64x64:d=1','-frames:v','1',video.name],check=True)
        contents=pathlib.Path(video.name).read_bytes()
        status,media=api('POST',f'/api/v1/chats/{chat_id}/files',token_a,contents,{'Content-Type':'video/mp4','X-File-Name':'video.mp4'})
        must(status,201,'video upload')
        must(api('POST',f'/api/v1/chats/{chat_id}/messages',token_a,{'client_message_id':str(uuid.uuid4()),'text':'Видео','attachment_ids':[media['id']]})[0],201,'video message')
        for _ in range(30):
            status,meta=api('GET',f"/api/v1/files/{media['id']}",token_b)
            must(status,200,'video metadata')
            if meta['preview_state']=='ready':break
            if meta['preview_state']=='failed':raise AssertionError('video preview failed')
            time.sleep(.3)
        else:raise AssertionError('video preview timed out')
        must(api('GET',f"/api/v1/files/{media['id']}/preview",token_b)[0],200,'recipient video preview')
    status, group = api("POST", "/api/v1/chats", token_a, {"title": "Семья"})
    must(status, 201, "group chat")
    group_id = group["id"]
    must(api("POST", f"/api/v1/chats/{group_id}/members", token_a, {"user_id": id_b})[0], 201, "add group member")
    status, group_file = api("POST", f"/api/v1/chats/{group_id}/files", token_a, b"group secret", {"Content-Type": "text/plain", "X-File-Name": "group.txt"})
    must(status, 201, "group upload")
    status, _ = api("POST", f"/api/v1/chats/{group_id}/messages", token_a, {"client_message_id": str(uuid.uuid4()), "text": "Файл группы", "attachment_ids": [group_file["id"]]})
    must(status, 201, "group message")
    group_path = f"/api/v1/files/{group_file['id']}/content"
    must(api("GET", group_path, token_b)[0], 200, "group member file")
    must(api("DELETE", f"/api/v1/chats/{group_id}/members/{id_b}", token_a)[0], 204, "remove member")
    must(api("GET", group_path, token_b)[0], 404, "removed member file")
    status, call = api("POST", f"/api/v1/chats/{chat_id}/calls", token_a, {"kind": "audio"})
    must(status, 201, "start call")
    call_id = call["id"]
    must(api("POST", f"/api/v1/calls/{call_id}/accept", token_b)[0], 200, "accept call")
    must(api("GET", f"/api/v1/calls/{call_id}/ice", token_b)[0], 200, "temporary TURN credentials")
    must(api("POST", f"/api/v1/calls/{call_id}/end", token_a)[0], 200, "end call")
    subprocess.run(["docker", "compose", "restart", "api"], check=True, stdout=subprocess.DEVNULL)
    for _ in range(30):
        try:
            if api("GET", "/health/ready")[0] == 200:
                break
        except Exception:
            pass
        time.sleep(.5)
    else:
        raise AssertionError("API did not return after restart")
    status, history = api("GET", f"/api/v1/chats/{chat_id}/messages", token_b)
    must(status, 200, "history after restart")
    if file_id not in json.dumps(history):
        raise AssertionError("attachment missing after restart")
    print("PASS: registration, admin, chats, private files, photo/video previews, search, calls, restart persistence")

if __name__ == "__main__":
    main()
