package main

// Общие хелперы файловых хранилищ (звуковая аптечка, галерея портретов).
// Вынесены из sounds.go/avatars.go: upload-логика дублировалась дословно.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// writeFileLimited: сохранить dir/<randHex()+ext>. Содержимое = head + остаток
// src, но не больше maxBytes. Атомарность в пределах best-effort: при ошибке
// записи файл удаляется. Возвращает имя созданного файла (id).
//
// Вызов должен быть под локом хранилища (лимиты считаются снаружи).
func writeFileLimited(dir, ext string, head []byte, src io.Reader, maxBytes int64) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	id := randHex() + ext
	path := filepath.Join(dir, id)
	dst, err := os.Create(path)
	if err != nil {
		return "", err
	}
	lr := io.LimitReader(io.MultiReader(bytes.NewReader(head), src), maxBytes)
	if _, err := io.Copy(dst, lr); err != nil {
		_ = dst.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return id, nil
}
