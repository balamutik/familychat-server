package worker

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"familychat/server/internal/database"
)

type fakePushSender struct {
	count   int
	invalid bool
}

func (f *fakePushSender) Send(_ context.Context, _, _ string, _ json.RawMessage) (bool, error) {
	f.count++
	return f.invalid, nil
}

func TestPushWorkerSendsAndDropsInvalidTokenOnly(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	ctx := t.Context()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var userID, sessionID, deviceID string
	err = db.QueryRow(ctx, `INSERT INTO users(login,password_hash) VALUES('push'||substring(replace(gen_random_uuid()::text,'-','') from 1 for 16),'hash') RETURNING id::text`).Scan(&userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	err = db.QueryRow(ctx, `INSERT INTO sessions(user_id,token_hash,expires_at) VALUES($1,decode(repeat('ab',32),'hex'),now()+interval '1 hour') RETURNING id::text`, userID).Scan(&sessionID)
	if err != nil {
		t.Fatal(err)
	}
	err = db.QueryRow(ctx, `INSERT INTO push_devices(session_id,user_id,client_server_id,device_id,alert_token,voip_token)
        VALUES($1,$2,gen_random_uuid(),gen_random_uuid(),$3,$4) RETURNING id::text`, sessionID, userID, "alert-token", "voip-token").Scan(&deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO push_jobs(device_id,kind,payload) VALUES($1,'alert','{"aps":{"alert":"hi"}}')`, deviceID); err != nil {
		t.Fatal(err)
	}
	sender := &fakePushSender{}
	w := Worker{DB: db, Push: sender}
	more, err := w.sendPush(ctx)
	if err != nil || !more || sender.count != 1 {
		t.Fatalf("first send: more=%v err=%v count=%d", more, err, sender.count)
	}
	if _, err = db.Exec(ctx, `INSERT INTO push_jobs(device_id,kind,payload) VALUES($1,'alert','{"aps":{"alert":"hi"}}')`, deviceID); err != nil {
		t.Fatal(err)
	}
	sender.invalid = true
	more, err = w.sendPush(ctx)
	if err != nil || !more {
		t.Fatalf("invalid token: more=%v err=%v", more, err)
	}
	var alert, voip *string
	if err = db.QueryRow(ctx, `SELECT alert_token,voip_token FROM push_devices WHERE id=$1`, deviceID).Scan(&alert, &voip); err != nil {
		t.Fatal(err)
	}
	if alert != nil || voip == nil || *voip != "voip-token" {
		t.Fatalf("tokens after invalid alert: alert=%v voip=%v", alert, voip)
	}
	if _, err = db.Exec(ctx, `INSERT INTO push_jobs(device_id,kind,payload) VALUES($1,'voip',jsonb_build_object('call_id',gen_random_uuid()::text))`, deviceID); err != nil {
		t.Fatal(err)
	}
	more, err = w.sendPush(ctx)
	if err != nil || !more || sender.count != 2 {
		t.Fatalf("stale call: more=%v err=%v count=%d", more, err, sender.count)
	}
}
