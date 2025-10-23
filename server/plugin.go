package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

type Plugin struct {
	plugin.MattermostPlugin

	confPath string
	mu       sync.RWMutex
	cfg      *Config
	httpc    *http.Client
}

type Config struct {
	// Провайдер генерации
	GenAPIEndpoint   string `json:"gen_api_endpoint"`   // например: https://api.gen-api.ru/v1/images/generate  (оставлено настраиваемым)
	GenAPIToken      string `json:"gen_api_token"`
	GenAPIModel      string `json:"gen_api_model"`      // "gpt-image-1"
	// pCloud
	PCloudAPIHost    string `json:"pcloud_api_host"`    // "https://api.pcloud.com"
	PCloudToken      string `json:"pcloud_token"`
	PCloudFolderID   int64  `json:"pcloud_folder_id"`   // куда грузим изображения
	// Квоты/лог
	DefaultMonthlyQuota int       `json:"default_monthly_quota"`
	NextReset           time.Time `json:"next_reset"`
	LogPath             string    `json:"log_path"`

	// Пользователи по MM user_id
	Users map[string]*User `json:"users"`
}

type User struct {
	Username        string     `json:"username"`
	UserID          string     `json:"user_id"`
	Admin           bool       `json:"admin"`
	MaxPerMonth     int        `json:"max_per_month"`
	UsedThisPeriod  int        `json:"used_this_period"`
	BonusThisPeriod int        `json:"bonus_this_period"`
	LastGeneration  *time.Time `json:"last_generation,omitempty"`
}

func (u *User) Remaining() int {
	return u.MaxPerMonth + u.BonusThisPeriod - u.UsedThisPeriod
}

func (p *Plugin) OnActivate() error {
	// Читаем путь к конфигу из настроек плагина
	conf := p.API.GetConfig()
	if conf == nil {
		return errors.New("failed to get Mattermost config")
	}
	p.confPath = p.getConfigString("ConfigFilePath")
	if p.confPath == "" {
		return errors.New("ConfigFilePath is empty in plugin settings")
	}
	if err := p.ensureConfig(); err != nil {
		return err
	}

	p.httpc = &http.Client{Timeout: 120 * time.Second}

	// Регистрируем команды
	for _, trigger := range []string{"imagelow", "imagemedium", "imagehigh"} {
		if err := p.registerCommand(trigger, "Сгенерировать изображение: /"+trigger+" <prompt>"); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		t, h string
	}{
		{"add_user", "/add_user <username> — добавить пользователя (админ)"},
		{"give_gen", "/give_gen <username|id> <amount> — выдать доп. генерации на текущий период (админ)"},
		{"my_quota", "/my_quota — показать мою квоту"},
		{"list_users", "/list_users — список пользователей (админ)"},
		{"remove_user", "/remove_user <username|id> — удалить (админ)"},
		{"show_log", "/show_log <n> — показать последние n строк лога (админ)"},
	} {
		if err := p.registerCommand(c.t, c.h); err != nil {
			return err
		}
	}

	p.API.LogInfo("imagegen plugin activated")
	return nil
}

func (p *Plugin) registerCommand(trigger, help string) error {
	cmd := &model.Command{
		Trigger:          trigger,
		AutoComplete:     true,
		AutoCompleteDesc: help,
		AutoCompleteHint: "",
	}
	return p.API.RegisterCommand(cmd)
}

func (p *Plugin) getConfigString(key string) string {
	settings := p.API.GetPluginConfig()
	if settings == nil {
		return ""
	}
	if v, ok := settings[key].(string); ok {
		return v
	}
	return ""
}

func (p *Plugin) ensureConfig() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Создать директорию
	if err := os.MkdirAll(filepath.Dir(p.confPath), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	// Если нет файла — создать дефолт
	if _, err := os.Stat(p.confPath); os.IsNotExist(err) {
		now := time.Now().UTC()
		def := &Config{
			GenAPIEndpoint:      "https://api.gen-api.ru/v1/images", // провайдер настраивается
			GenAPIToken:         "PUT_YOUR_TOKEN",
			GenAPIModel:         "gpt-image-1",
			PCloudAPIHost:       "https://api.pcloud.com",
			PCloudToken:         "PUT_PCLOUD_TOKEN",
			PCloudFolderID:      0,
			DefaultMonthlyQuota: 20,
			NextReset:           now.AddDate(0, 1, 0),
			LogPath:             filepath.Join(filepath.Dir(p.confPath), "imagegen.log"),
			Users:               map[string]*User{},
		}
		if err := writeJSON(p.confPath, def); err != nil {
			return err
		}
		p.cfg = def
		return nil
	}
	// Иначе загрузить
	var cfg Config
	if err := readJSON(p.confPath, &cfg); err != nil {
		return err
	}
	if cfg.Users == nil {
		cfg.Users = map[string]*User{}
	}
	p.cfg = &cfg
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func (p *Plugin) logf(format string, args ...any) {
	msg := fmt.Sprintf(time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
	p.API.LogInfo(msg)
	p.mu.RLock()
	logPath := ""
	if p.cfg != nil {
		logPath = p.cfg.LogPath
	}
	p.mu.RUnlock()
	if logPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		defer f.Close()
		_, _ = f.WriteString(msg)
	}
}

func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	trigger := strings.TrimPrefix(strings.SplitN(args.Command, " ", 2)[0], "/")
	text := strings.TrimSpace(strings.TrimPrefix(args.Command, "/"+trigger))
	p.logf("REQ trigger=%s user=%s text=%q", trigger, args.UserId, text)

	switch trigger {
	case "imagelow", "imagemedium", "imagehigh":
		quality := map[string]string{"imagelow": "low", "imagemedium": "medium", "imagehigh": "high"}[trigger]
		return p.handleGenerate(args, text, quality)
	case "add_user":
		return p.handleAddUser(args, text)
	case "give_gen":
		return p.handleGiveGen(args, text)
	case "my_quota":
		return p.handleMyQuota(args)
	case "list_users":
		return p.handleListUsers(args)
	case "remove_user":
		return p.handleRemoveUser(args, text)
	case "show_log":
		return p.handleShowLog(args, text)
	default:
		return &model.CommandResponse{ResponseType: model.CommandResponseTypeEphemeral, Text: "Unknown command"}, nil
	}
}

func (p *Plugin) handleGenerate(args *model.CommandArgs, prompt string, quality string) (*model.CommandResponse, *model.AppError) {
	if strings.TrimSpace(prompt) == "" {
		return resp("Нужно указать промпт: `/"+strings.ReplaceAll(args.Command, "/"+quality, quality)+" <prompt>`"), nil
	}
	// Обновить/сбросить квоты
	if err := p.withConfig(func(cfg *Config) error {
		now := time.Now().UTC()
		if now.After(cfg.NextReset) || now.Equal(cfg.NextReset) {
			for _, u := range cfg.Users {
				u.UsedThisPeriod = 0
				u.BonusThisPeriod = 0
			}
			cfg.NextReset = cfg.NextReset.AddDate(0, 1, 0)
			p.logf("quota reset executed; next=%s", cfg.NextReset.Format(time.RFC3339))
		}
		// Авто-регистрация пользователя, если не найден
		u := cfg.Users[args.UserId]
		if u == nil {
			mmUser, appErr := p.API.GetUser(args.UserId)
			if appErr != nil {
				return fmt.Errorf("get user: %v", appErr)
			}
			u = &User{
				Username:        mmUser.Username,
				UserID:          args.UserId,
				Admin:           false,
				MaxPerMonth:     cfg.DefaultMonthlyQuota,
				UsedThisPeriod:  0,
				BonusThisPeriod: 0,
			}
			cfg.Users[args.UserId] = u
		}
		if u.Remaining() <= 0 {
			return &quotaError{msg: fmt.Sprintf("Квота исчерпана. Сброс: %s. Остаток: 0", cfg.NextReset.Format("2006-01-02 15:04"))}
		}
		return nil
	}); err != nil {
		if qe, ok := err.(*quotaError); ok {
			return resp(qe.msg), nil
		}
		return resp("Ошибка квоты: "+err.Error()), nil
	}

	// Генерация
	imgBytes, genInfo, err := p.generateImage(context.Background(), prompt, quality)
	if err != nil {
		p.logf("ERR generate: %v", err)
		return resp("Не удалось сгенерировать изображение: " + err.Error()), nil
	}

	// Загрузка в pCloud
	pubURL, err := p.uploadToPCloudAndGetPublicURL(imgBytes)
	if err != nil {
		p.logf("ERR pcloud: %v", err)
		return resp("Сгенерировал, но не смог опубликовать в pCloud: " + err.Error()), nil
	}

	// Списать квоту
	_ = p.withConfig(func(cfg *Config) error {
		u := cfg.Users[args.UserId]
		now := time.Now().UTC()
		u.UsedThisPeriod++
		u.LastGeneration = &now
		return nil
	})

	p.logf("OK gen -> %s (quality=%s size=%dB info=%s)", pubURL, quality, len(imgBytes), genInfo)
	return resp(pubURL), nil
}

type quotaError struct{ msg string }
func (e *quotaError) Error() string { return e.msg }

func (p *Plugin) handleAddUser(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	parts := fields(text)
	if len(parts) != 1 {
		return resp("Синтаксис: /add_user <username>"), nil
	}
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора."), nil
	}
	username := strings.TrimSpace(parts[0])
	mmUser, appErr := p.API.GetUserByUsername(username)
	if appErr != nil {
		return resp("Пользователь не найден в Mattermost: " + username), nil
	}
	err := p.withConfig(func(cfg *Config) error {
		if _, exists := cfg.Users[mmUser.Id]; exists {
			return fmt.Errorf("пользователь уже добавлен")
		}
		cfg.Users[mmUser.Id] = &User{
			Username:        mmUser.Username,
			UserID:          mmUser.Id,
			Admin:           false,
			MaxPerMonth:     cfg.DefaultMonthlyQuota,
			UsedThisPeriod:  0,
			BonusThisPeriod: 0,
		}
		return nil
	})
	if err != nil {
		return resp("Ошибка: " + err.Error()), nil
	}
	return resp("Добавлен: " + username), nil
}

func (p *Plugin) handleGiveGen(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора."), nil
	}
	parts := fields(text)
	if len(parts) != 2 {
		return resp("Синтаксис: /give_gen <username|id> <amount>"), nil
	}
	target := parts[0]
	amt, err := strconv.Atoi(parts[1])
	if err != nil || amt <= 0 {
		return resp("amount должен быть положительным числом"), nil
	}
	return p.findUserAndDo(target, func(u *User) error {
		u.BonusThisPeriod += amt
		return nil
	}, fmt.Sprintf("Выдано +%d генераций пользователю ", amt))
}

func (p *Plugin) handleMyQuota(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	var msg string
	_ = p.withConfig(func(cfg *Config) error {
		u := cfg.Users[args.UserId]
		if u == nil {
			msg = fmt.Sprintf("Вы ещё не добавлены. Базовая квота: %d. Сброс: %s",
				cfg.DefaultMonthlyQuota, cfg.NextReset.Format("2006-01-02 15:04"))
			return nil
		}
		msg = fmt.Sprintf("Остаток: %d (использовано %d из %d, бонус %d). Сброс: %s",
			u.Remaining(), u.UsedThisPeriod, u.MaxPerMonth, u.BonusThisPeriod, cfg.NextReset.Format("2006-01-02 15:04"))
		return nil
	})
	return resp(msg), nil
}

func (p *Plugin) handleListUsers(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора."), nil
	}
	var b strings.Builder
	_ = p.withConfig(func(cfg *Config) error {
		fmt.Fprintf(&b, "Пользователи (%d):\n", len(cfg.Users))
		for _, u := range cfg.Users {
			last := ""
			if u.LastGeneration != nil {
				last = u.LastGeneration.Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&b, "- %s (id=%s) admin=%v, остаток=%d, использовано=%d, макс=%d, бонус=%d, last=%s\n",
				u.Username, u.UserID, u.Admin, u.Remaining(), u.UsedThisPeriod, u.MaxPerMonth, u.BonusThisPeriod, last)
		}
		return nil
	})
	return resp(b.String()), nil
}

func (p *Plugin) handleRemoveUser(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора."), nil
	}
	parts := fields(text)
	if len(parts) != 1 {
		return resp("Синтаксис: /remove_user <username|id>"), nil
	}
	target := parts[0]
	return p.findUserAndDo(target, func(u *User) error {
		return p.withConfig(func(cfg *Config) error {
			delete(cfg.Users, u.UserID)
			return nil
		})
	}, "Удалён пользователь ")
}

func (p *Plugin) handleShowLog(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора."), nil
	}
	n := 100
	if t := strings.TrimSpace(text); t != "" {
		if v, err := strconv.Atoi(t); err == nil && v > 0 {
			n = v
		}
	}
	p.mu.RLock()
	lp := p.cfg.LogPath
	p.mu.RUnlock()
	lines, err := tailLastLines(lp, n)
	if err != nil {
		return resp("Ошибка чтения лога: " + err.Error()), nil
	}
	return resp("```\n" + strings.Join(lines, "\n") + "\n```"), nil
}

func (p *Plugin) isAdmin(userID string) bool {
	ok := false
	_ = p.withConfig(func(cfg *Config) error {
		if u := cfg.Users[userID]; u != nil {
			ok = u.Admin
		}
		return nil
	})
	return ok
}

func (p *Plugin) findUserAndDo(target string, fn func(*User) error, okPrefix string) (*model.CommandResponse, *model.AppError) {
	var out string
	err := p.withConfig(func(cfg *Config) error {
		// по id?
		if u := cfg.Users[target]; u != nil {
			if err := fn(u); err != nil {
				return err
			}
			out = okPrefix + u.Username
			return nil
		}
		// по username
		for _, u := range cfg.Users {
			if u.Username == target {
				if err := fn(u); err != nil {
					return err
				}
				out = okPrefix + u.Username
				return nil
			}
		}
		return fmt.Errorf("пользователь не найден")
	})
	if err != nil {
		return resp("Ошибка: " + err.Error()), nil
	}
	return resp(out), nil
}

func (p *Plugin) withConfig(edit func(*Config) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg == nil {
		return errors.New("config not loaded")
	}
	if err := edit(p.cfg); err != nil {
		return err
	}
	return writeJSON(p.confPath, p.cfg)
}

func fields(s string) []string {
	f := strings.Fields(s)
	out := make([]string, 0, len(f))
	// поддержим quoted строки "..."
	var buf []rune
	inQ := false
	for _, r := range s {
		switch r {
		case '"':
			if inQ {
				out = append(out, strings.TrimSpace(string(buf)))
				buf = buf[:0]
				inQ = false
			} else {
				inQ = true
			}
		case ' ':
			if inQ {
				buf = append(buf, r)
			} else if len(buf) > 0 {
				out = append(out, strings.TrimSpace(string(buf)))
				buf = buf[:0]
			}
		default:
			buf = append(buf, r)
		}
	}
	if len(buf) > 0 {
		out = append(out, strings.TrimSpace(string(buf)))
	}
	if len(out) == 0 {
		return f // fallback
	}
	return out
}

func resp(text string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         text,
	}
}

// ============ Генерация через GenAPI (абстрактно, т.к. у GenAPI детали после логина) ============
type genAPIResponse struct {
	// OpenAI-like
	Data []struct {
		B64 string `json:"b64_json"`
		URL string `json:"url"`
	} `json:"data"`
	// or custom wrapper
	ImageB64 string `json:"image_b64"`
	Error    string `json:"error"`
}

func (p *Plugin) generateImage(ctx context.Context, prompt string, quality string) ([]byte, string, error) {
	p.mu.RLock()
	endpoint := p.cfg.GenAPIEndpoint
	token := p.cfg.GenAPIToken
	modelID := p.cfg.GenAPIModel
	p.mu.RUnlock()

	// маппинг качества -> "size"/"quality"
	size := map[string]string{"low": "512x512", "medium": "1024x1024", "high": "2048x2048"}[quality]
	if size == "" {
		size = "1024x1024"
	}

	payload := map[string]any{
		"model":  modelID,
		"prompt": prompt,
		"size":   size,
		"n":      1,
		"quality": quality, // если у провайдера есть этот параметр — пусть передастся
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("gen-api http %d: %s", resp.StatusCode, string(b))
	}
	var g genAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return nil, "", err
	}
	if g.Error != "" {
		return nil, "", errors.New(g.Error)
	}

	// приоритет: b64
	if len(g.Data) > 0 && g.Data[0].B64 != "" {
		img, err := base64.StdEncoding.DecodeString(g.Data[0].B64)
		return img, "b64", err
	}
	if g.ImageB64 != "" {
		img, err := base64.StdEncoding.DecodeString(g.ImageB64)
		return img, "b64", err
	}
	// если отдают URL — стянем
	var url string
	if len(g.Data) > 0 && g.Data[0].URL != "" {
		url = g.Data[0].URL
	}
	if url == "" {
		return nil, "", errors.New("gen-api: empty image")
	}
	r2, err := p.httpc.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer r2.Body.Close()
	if r2.StatusCode >= 300 {
		b, _ := io.ReadAll(r2.Body)
		return nil, "", fmt.Errorf("download http %d: %s", r2.StatusCode, string(b))
	}
	img, err := io.ReadAll(r2.Body)
	return img, "url", err
}

// ============ pCloud ============
type pcloudUploadResp struct {
	Result   int `json:"result"`
	Metadata []struct {
		FileID int64  `json:"fileid"`
		Name   string `json:"name"`
	} `json:"metadata"`
	Error string `json:"error"`
}

type pcloudPublinkResp struct {
	Result int    `json:"result"`
	Code   string `json:"code"`
	Link   string `json:"link"`
	Error  string `json:"error"`
}

func (p *Plugin) uploadToPCloudAndGetPublicURL(img []byte) (string, error) {
	p.mu.RLock()
	host := p.cfg.PCloudAPIHost
	token := p.cfg.PCloudToken
	fid := p.cfg.PCloudFolderID
	p.mu.RUnlock()

	// 1) uploadfile
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("access_token", token)
	_ = w.WriteField("folderid", fmt.Sprintf("%d", fid))
	_ = w.WriteField("renameifexists", "1")
	fw, _ := w.CreateFormFile("file", fmt.Sprintf("image-%d.png", time.Now().Unix()))
	_, _ = fw.Write(img)
	_ = w.Close()

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(host, "/")+"/uploadfile", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := p.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pcloud upload http %d: %s", resp.StatusCode, string(b))
	}
	var up pcloudUploadResp
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		return "", err
	}
	if up.Result != 0 || len(up.Metadata) == 0 {
		return "", fmt.Errorf("pcloud upload error: %s (code=%d)", up.Error, up.Result)
	}
	fileID := up.Metadata[0].FileID

	// 2) getfilepublink
	u2 := fmt.Sprintf("%s/getfilepublink?access_token=%s&fileid=%d", strings.TrimRight(host, "/"), token, fileID)
	r2, err := p.httpc.Get(u2)
	if err != nil {
		return "", err
	}
	defer r2.Body.Close()
	var pl pcloudPublinkResp
	if err := json.NewDecoder(r2.Body).Decode(&pl); err != nil {
		return "", err
	}
	if pl.Result != 0 || pl.Link == "" {
		return "", fmt.Errorf("pcloud publink error: %s (code=%d)", pl.Error, pl.Result)
	}
	// pl.Link — это страница, у которой внутри есть прямой dl; для простоты вернём pl.Link
	return pl.Link, nil
}

// ===== утилита tail =====
func tailLastLines(path string, n int) ([]string, error) {
	if n <= 0 {
		n = 100
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return lines, nil
	}
	return lines[len(lines)-n:], nil
}

func main() {
	plugin.ClientMain(&Plugin{})
}