package main

import (
	"bytes"
	"context"
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
	"regexp"
	"sync"
	"time"
	"net/url"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

type configuration struct {
    ConfigFilePath string
}

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
	GenAPIGetEndpoint string `json:"gen_api_get_endpoint"`
	GenAPIToken      string `json:"gen_api_token"`
	GenAPIModel      string `json:"gen_api_model"`      // "gpt-image-1"
	// pCloud
	PCloudAPIHost    string `json:"pcloud_api_host"`    // "https://api.pcloud.com"
	PCloudUsername string `json:"pcloud_user"`
	PCloudPassword string `json:"pcloud_password"`
	PCloudBearerToken string `json:"pcloud_bearer_token"`
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

func (p *Plugin) OnConfigurationChange() error {
    var c configuration
    if err := p.API.LoadPluginConfiguration(&c); err != nil {
        return err
    }
    p.confPath = strings.TrimSpace(c.ConfigFilePath)
    return nil
}

func (p *Plugin) OnActivate() error {
    if err := p.OnConfigurationChange(); err != nil {
        return err
    }
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
			GenAPIEndpoint:      "https://api.gen-api.ru/api/v1/networks/", // провайдер настраивается
			GenAPIGetEndpoint:   "https://api.gen-api.ru/api/v1/request/get/",
			GenAPIToken:         "PUT_YOUR_TOKEN",
			GenAPIModel:         "qwen-image",
			PCloudAPIHost:       "https://eapi.pcloud.com",
			PCloudUsername: 	 "PUT_PCLOUD_USERNAME",
	        PCloudPassword:     "PUT_PCLOUD_PASSWORD",
			PCloudBearerToken:  "PUT_PCLOUD_BEARER_TOKEN",
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

func (p *Plugin) riskyLogf(format string, args ...any) {
	msg := fmt.Sprintf(time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
	p.API.LogInfo(msg)
	logPath := ""
	if p.cfg != nil {
		logPath = p.cfg.LogPath
	}
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
		return resp("Нужно указать промпт: `/image"+quality+" <prompt>`", args.ChannelId), nil
	}
	// Обновить/сбросить квоты
	if err := p.withConfig(func(cfg *Config) error {
		now := time.Now().UTC()
		if now.After(cfg.NextReset) || now.Equal(cfg.NextReset) {
			for _, u := range cfg.Users {
				u.UsedThisPeriod = 0
				// u.BonusThisPeriod = 0
			}
			for !cfg.NextReset.After(now) {
				cfg.NextReset = cfg.NextReset.AddDate(0, 1, 0)
			}
			p.riskyLogf("quota reset executed; next=%s", cfg.NextReset.Format(time.RFC3339))
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
		p.logf("Err occured: %#v", err)
		if qe, ok := err.(*quotaError); ok {
			return resp(qe.msg, args.ChannelId), nil
		}
		return resp("Ошибка квоты: "+err.Error(), args.ChannelId), nil
	}

	var lastGen *time.Time

	// Списать квоту
	_ = p.withConfig(func(cfg *Config) error {
		u := cfg.Users[args.UserId]
		if u == nil {
			return fmt.Errorf("Пользователь был удалён")
		}
		now := time.Now().UTC()
		u.UsedThisPeriod++
		lastGen = u.LastGeneration
		u.LastGeneration = &now
		return nil
	})

	// Генерация
	imgBytes, genInfo, err := p.generateImage(context.Background(), prompt, quality)
	if err != nil {
		p.logf("ERR generate: %v", err)

		// Вернуть квоту
		_ = p.withConfig(func(cfg *Config) error {
			u := cfg.Users[args.UserId]
			if u == nil {
				return fmt.Errorf("Пользователь был удалён")
			}
			u.UsedThisPeriod--
			u.LastGeneration = lastGen
			return nil
		})

		return resp("Не удалось сгенерировать изображение: " + err.Error(), args.ChannelId), nil
	}

	// Загрузка в pCloud
	pubURL, err := p.uploadToPCloudAndGetPublicURL(imgBytes)
	if err != nil {
		p.logf("ERR pcloud: %v", err)
		return resp("Сгенерировал, но не смог опубликовать в pCloud: " + err.Error(), args.ChannelId), nil
	}

	p.logf("OK gen -> %s (quality=%s size=%dB info=%s)", pubURL, quality, len(imgBytes), genInfo)
	return resp(pubURL, args.ChannelId), nil
}

type quotaError struct{ msg string }
func (e *quotaError) Error() string { return e.msg }

func (p *Plugin) handleAddUser(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	parts := fields(text)
	if len(parts) != 1 {
		return resp("Синтаксис: /add_user <username>", args.ChannelId), nil
	}
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора.", args.ChannelId), nil
	}
	username := strings.TrimSpace(parts[0])
	mmUser, appErr := p.API.GetUserByUsername(username)
	if appErr != nil {
		return resp("Пользователь не найден в Mattermost: " + username, args.ChannelId), nil
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
		return resp("Ошибка: " + err.Error(), args.ChannelId), nil
	}
	return resp("Добавлен: " + username, args.ChannelId), nil
}

func (p *Plugin) handleGiveGen(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора.", args.ChannelId), nil
	}
	parts := fields(text)
	if len(parts) != 2 {
		return resp("Синтаксис: /give_gen <username|id> <amount>", args.ChannelId), nil
	}
	target := parts[0]
	amt, err := strconv.Atoi(parts[1])
	if err != nil || amt < 0 {
		return resp("amount должен быть неотрицательным числом", args.ChannelId), nil
	}
	return p.findUserAndDo(args.ChannelId, target, func(u *User, cfg *Config) error {
		u.BonusThisPeriod = amt
		return nil
	}, fmt.Sprintf("Выдано %d генераций пользователю ", amt))
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
	return resp(msg, args.ChannelId), nil
}

func (p *Plugin) handleListUsers(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора.", args.ChannelId), nil
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
	return resp(b.String(), args.ChannelId), nil
}

func (p *Plugin) handleRemoveUser(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора.", args.ChannelId), nil
	}
	parts := fields(text)
	if len(parts) != 1 {
		return resp("Синтаксис: /remove_user <username|id>", args.ChannelId), nil
	}
	target := parts[0]
	return p.findUserAndDo(args.ChannelId, target, func(u *User, cfg *Config) error {
		delete(cfg.Users, u.UserID)
		return nil
	}, "Удалён пользователь ")
}

func (p *Plugin) handleShowLog(args *model.CommandArgs, text string) (*model.CommandResponse, *model.AppError) {
	if !p.isAdmin(args.UserId) {
		return resp("Доступ только для администратора.", args.ChannelId), nil
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
		return resp("Ошибка чтения лога: " + err.Error(), args.ChannelId), nil
	}
	return resp("```\n" + strings.Join(lines, "\n") + "\n```", args.ChannelId), nil
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

func (p *Plugin) findUserAndDo(cid string, target string, fn func(*User, *Config) error, okPrefix string) (*model.CommandResponse, *model.AppError) {
	var out string
	err := p.withConfig(func(cfg *Config) error {
		// по id?
		if u := cfg.Users[target]; u != nil {
			if err := fn(u, cfg); err != nil {
				return err
			}
			out = okPrefix + u.Username
			return nil
		}
		// по username
		for _, u := range cfg.Users {
			if u.Username == target {
				if err := fn(u, cfg); err != nil {
					return err
				}
				out = okPrefix + u.Username
				return nil
			}
		}
		return fmt.Errorf("пользователь не найден")
	})
	if err != nil {
		return resp("Ошибка: " + err.Error(), cid), nil
	}
	return resp(out, cid), nil
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

func resp(text string, cid string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeInChannel,
		Text:         text,
		ChannelId:    cid,
	}
}

// ============ Генерация через GenAPI ============
type genAPIResponse struct {
	RID 	 int64 `json:"request_id"`
	Status   string `json:"status"`
}

type genAPIResult struct {
	Result 	 []string `json:"result"`
	Status   string `json:"status"`
}

func (p *Plugin) requestImage(ctx context.Context, resId int64, token string, getUrl string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getUrl+fmt.Sprintf("%d", resId), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var g genAPIResult
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return "", err
	}

	if g.Status != "success" {
		return "", errors.New(g.Status)
	}

	return g.Result[0], nil
}

func (p *Plugin) generateImage(ctx context.Context, prompt string, quality string) ([]byte, string, error) {
	p.mu.RLock()
	endpoint := p.cfg.GenAPIEndpoint
	getUrl := p.cfg.GenAPIGetEndpoint
	token := p.cfg.GenAPIToken
	modelID := p.cfg.GenAPIModel
	p.mu.RUnlock()

	var payload map[string]any
	if modelID == "qwen-image" {
		// маппинг качества -> "size"/"quality"
		size, ok := map[string][2]int{"low": {512, 512}, "medium": {1024, 1024}, "high": {1536, 1536}}[quality]
		if !ok {
			size = [2]int{1024, 1024}
		}

		imageWidth := size[0]
		imageHeight := size[1]

		payload = map[string]any{
			"prompt": 					prompt,
			"width":  					imageWidth,
			"height": 					imageHeight,
			"num_images":				1,
			"enable_safety_checker": 	false,
			"negative_prompt":			"-",
		}
	} else if modelID == "gpt-image-1" {
		payload = map[string]any{
			"prompt": 		prompt,
			"quality":  	quality,
			"is_sync":		false,
		}
	} else {
		payload = map[string]any{
			"prompt": 		prompt,
			"quality":  	quality,
			"is_sync":		false,
			"model": 		modelID,
		}

		modelID = "gpt-image-1"
	}


	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+modelID, bytes.NewReader(body))
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

	const (
        maxAttempts    = 20
        pollInterval   = 2 * time.Second
    )

	var imUrl string
    for i := 0; i < maxAttempts; i++ {
        select {
        case <-ctx.Done():
            return nil, "", ctx.Err()
        case <-time.After(pollInterval):
        }

        imUrl, err = p.requestImage(ctx, g.RID, token, getUrl)
		if err == nil {
			break
		}
	}

	// если отдают URL — стянем
	if imUrl == "" {
		return nil, "", errors.New("gen-api: empty image")
	}
	r2, err := p.httpc.Get(imUrl)
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
type pcloudTokenResp struct {
	Token 	 string `json:"auth"`
	Result   int `json:"result"`
	Error 	 string `json:"error"`
}

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
	username := p.cfg.PCloudUsername
	password := p.cfg.PCloudPassword
	btoken := p.cfg.PCloudBearerToken
	has_btoken := btoken != "" && btoken != "PUT_PCLOUD_BEARER_TOKEN" && btoken != "YOUR_PCLOUD_BEARER_TOKEN"
	fid := p.cfg.PCloudFolderID
	p.mu.RUnlock()

	var token string

	if !has_btoken {

		// 0) gettoken

		base, _ := url.Parse(strings.TrimRight(host, "/")+"/userinfo")

		// Query parameters
		params := url.Values{}
		params.Set("getauth", "1")
		params.Set("logout", "1")
		params.Set("username", username)
		params.Set("password", password)

		// This encodes all special chars (+, @, &, spaces, etc.)
		base.RawQuery = params.Encode()

		req, err := http.NewRequest(http.MethodGet, base.String(), nil)
		if err != nil {
			safe := sanitizePCloudError(err)
			return "", fmt.Errorf("pcloud http error: %s", safe)
		}

		resp, err := p.httpc.Do(req)
		if err != nil {
			safe := sanitizePCloudError(err)
			return "", fmt.Errorf("pcloud http error: %s", safe)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			return "", fmt.Errorf("pcloud get token http %d: %s", resp.StatusCode, sanitizePCloudPayload(string(b)))
		}

		var ptr pcloudTokenResp
		if err := json.NewDecoder(resp.Body).Decode(&ptr); err != nil {
			safe := sanitizePCloudError(err)
			return "", fmt.Errorf("pcloud decode error: %s", safe)
		}

		if ptr.Result != 0 {
			return "", fmt.Errorf("pcloud auth error: %s (code=%d)", ptr.Error, ptr.Result)
		}

		token = ptr.Token

	} else {
		token = "0"
	}

	// 1) uploadfile
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if !has_btoken {
		_ = w.WriteField("auth", token)
	}
	_ = w.WriteField("folderid", fmt.Sprintf("%d", fid))
	_ = w.WriteField("renameifexists", "1")
	fw, _ := w.CreateFormFile("file", fmt.Sprintf("image-%d.png", time.Now().Unix()))
	_, _ = fw.Write(img)
	_ = w.Close()

	reqN, errN := http.NewRequest(http.MethodPost, strings.TrimRight(host, "/")+"/uploadfile", &buf)
	if errN != nil {
		safe := sanitizePCloudError(errN)
		return "", fmt.Errorf("pcloud http error: %s", safe)
	}

	reqN.Header.Set("Content-Type", w.FormDataContentType())
	if has_btoken {
		reqN.Header.Set("Authorization", "Bearer "+btoken)
	}

	respN, errN := p.httpc.Do(reqN)
	if errN != nil {
		safe := sanitizePCloudError(errN)
		return "", fmt.Errorf("pcloud http error: %s", safe)
	}
	defer respN.Body.Close()
	if respN.StatusCode >= 300 {
		b, _ := io.ReadAll(respN.Body)
		return "", fmt.Errorf("pcloud upload http %d: %s", respN.StatusCode, sanitizePCloudPayload(string(b)))
	}
	var up pcloudUploadResp
	if err := json.NewDecoder(respN.Body).Decode(&up); err != nil {
		safe := sanitizePCloudError(err)
		return "", fmt.Errorf("pcloud decode error: %s", safe)
	}
	if up.Result != 0 || len(up.Metadata) == 0 {
		return "", fmt.Errorf("pcloud upload error: %s (code=%d)", up.Error, up.Result)
	}
	fileID := up.Metadata[0].FileID

	// 2) getfilepublink
	var u2 string
	if !has_btoken {
		u2 = fmt.Sprintf("%s/getfilepublink?auth=%s&fileid=%d", strings.TrimRight(host, "/"), token, fileID)
	} else {
		u2 = fmt.Sprintf("%s/getfilepublink?fileid=%d", strings.TrimRight(host, "/"), fileID)
	}
	req2, err := http.NewRequest(http.MethodGet, u2, nil)
	if err != nil {
		safe := sanitizePCloudError(err)
		return "", fmt.Errorf("pcloud http error: %s", safe)
	}

	if has_btoken {
		req2.Header.Set("Authorization", "Bearer "+btoken)
	}

	r2, err := p.httpc.Do(req2)
	if err != nil {
		safe := sanitizePCloudError(err)
		return "", fmt.Errorf("pcloud http error: %s", safe)
	}
	defer r2.Body.Close()
	var pl pcloudPublinkResp
	if err := json.NewDecoder(r2.Body).Decode(&pl); err != nil {
		safe := sanitizePCloudError(err)
		return "", fmt.Errorf("pcloud decode error: %s", safe)
	}
	if pl.Result != 0 || pl.Link == "" {
		return "", fmt.Errorf("pcloud publink error: %s (code=%d)", pl.Error, pl.Result)
	}
	// pl.Link — это страница, у которой внутри есть прямой dl; для простоты вернём pl.Link
	return pl.Link, nil
}

var (
	rePass = regexp.MustCompile(`password=[^&"\s]+`)
	reUser = regexp.MustCompile(`username=[^&"\s]+`)
	reToken = regexp.MustCompile(`auth=[^&"\s]+`)
)

// sanitizePCloudError делает текст ошибки безопасным для логов/юзера
func sanitizePCloudError(err error) string {
	if err == nil {
		return ""
	}

	// 1) Аккуратно обрабатываем url.Error, чтобы выпилить креды из URL
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if parsed, perr := url.Parse(uerr.URL); perr == nil {
			q := parsed.Query()
			if q.Has("password") {
				q.Set("password", "***")
			}
			if q.Has("username") {
				q.Set("username", "***")
			}
			if q.Has("auth") {
				q.Set("auth", "***")
			}
			parsed.RawQuery = q.Encode()

			return fmt.Sprintf("%s %s: %v", uerr.Op, parsed.String(), uerr.Err)
		}
	}

	// 2) Fallback — просто выпиливаем креды из строки
	s := err.Error()
	s = rePass.ReplaceAllString(s, "password=***")
	s = reUser.ReplaceAllString(s, "username=***")
	s = reToken.ReplaceAllString(s, "auth=***")
	return s
}

func sanitizePCloudPayload(pl string) string {
	if pl == "" {
		return ""
	}

	s := pl
	s = rePass.ReplaceAllString(s, "password=***")
	s = reUser.ReplaceAllString(s, "username=***")
	s = reToken.ReplaceAllString(s, "auth=***")
	return s
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