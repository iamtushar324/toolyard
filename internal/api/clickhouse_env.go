package api

import (
	"fmt"
	"os"
)

// WriteClickhouseRuntimeEnv emits the env file the toolyard-clickhouse
// docker-compose stack reads. Written atomically (temp + rename) so a
// partial write can never be observed mid-update by a CH restart. Mode
// 0640 so root-via-sudo (the typical docker compose runner) can read it
// while unrelated local users cannot. path == "" disables — useful in
// tests and on first boot before the operator has decided where the file
// should live.
//
// Exported so cmd/gateway can call it during the clickhouse_password
// bootstrap without instantiating an api.Server.
func WriteClickhouseRuntimeEnv(filePath, password string) error {
	if filePath == "" {
		return nil
	}
	tmp := filePath + ".tmp"
	body := fmt.Sprintf("# managed by toolyard — do not edit by hand. Rotate via the dashboard.\nTOOLYARD_CH_PASSWORD=%s\n", password)
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filePath)
}
