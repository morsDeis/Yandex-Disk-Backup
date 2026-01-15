package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	baseURL          = "https://cloud-api.yandex.net/v1/disk/resources"
	discordAvatarURL = "https://raw.githubusercontent.com/google/material-design-icons/refs/heads/master/png/action/backup/materialicons/48dp/2x/baseline_backup_black_48dp.png"
	discordBotName   = "Backup System"
)

// --- ГЛОБАЛЬНЫЕ ПЕРЕМЕННЫЕ ---
var (
	// Пути (определяются в init)
	LocalBackupDir  string
	DockerPath      string
	SillyPath       string
	UfwRulesPath    = "/etc/ufw/user.rules"
	RemoteBackupDir = "/backup/"

	// Вебхук (определяется через флаг)
	discordWebhookURL string
)

func init() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Ошибка определения домашней директории: %v\n", err)
		os.Exit(1)
	}

	LocalBackupDir = filepath.Join(homeDir, "backups")
	DockerPath = filepath.Join(homeDir, "docker")
	SillyPath = filepath.Join(homeDir, "docker", "sillytavern")
}

// Структуры для Яндекса
type LinkResponse struct {
	Href string `json:"href"`
}

type ResourceList struct {
	Embedded struct {
		Items []ResourceItem `json:"items"`
	} `json:"_embedded"`
}

type ResourceItem struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Created time.Time `json:"created"`
	Type    string    `json:"type"`
}

// Структура для Discord
type DiscordPayload struct {
	Content   string `json:"content"`
	Username  string `json:"username"`
	AvatarUrl string `json:"avatar_url"`
}

func main() {
	mode := flag.String("mode", "", "Режим: 'upload', 'download' или 'backup'")
	token := flag.String("token", "", "OAuth токен Яндекс.Диска")
	pass := flag.String("password", "", "Пароль для архива (только для mode=backup)")
	
	// Параметры для upload/download
	filePath := flag.String("file", "", "Путь к файлу (для upload)")
	prefix := flag.String("prefix", "", "Префикс поиска (для download)")
	remotePath := flag.String("remote", "", "Папка на Диске (необязательно)")

	// Новый флаг для вебхука (привязываем прямо к глобальной переменной)
	flag.StringVar(&discordWebhookURL, "webhook", "", "URL вебхука Discord (если пустой, уведомления отключены)")

	flag.Parse()

	if *token == "" {
		fatalError("Не указан токен (-token)")
	}

	client := &http.Client{Timeout: 60 * time.Second}

	switch *mode {
	case "upload":
		targetRemote := *remotePath
		if targetRemote == "" { targetRemote = "/" }
		if *filePath == "" { fatalError("Не указан файл (-file)") }
		uploadFile(client, *token, *filePath, targetRemote)
		
	case "download":
		targetRemote := *remotePath
		if targetRemote == "" { targetRemote = "/" }
		if *prefix == "" { fatalError("Не указан префикс (-prefix)") }
		downloadNewest(client, *token, *prefix, targetRemote)
		
	case "backup":
		if *pass == "" { fatalError("Не указан пароль для архивации (-password)") }
		runFullBackup(client, *token, *pass)
		
	default:
		fatalError("Неверный режим. Используйте -mode=backup, -mode=upload или -mode=download")
	}
}

// --- ЛОГИКА БЕКАПА ---
func runFullBackup(client *http.Client, token, password string) {
	dateStr := time.Now().Format("20060102_150405")
	tmpDir := filepath.Join(os.TempDir(), "backup_work")

	mainArchiveName := fmt.Sprintf("backup_main-%s.7z", dateStr)
	sillyArchiveName := fmt.Sprintf("backup_silly-%s.7z", dateStr)
	
	mainArchivePath := filepath.Join(LocalBackupDir, mainArchiveName)
	sillyArchivePath := filepath.Join(LocalBackupDir, sillyArchiveName)

	fmt.Println("=== Запуск процесса архивации ===")

	if err := os.MkdirAll(LocalBackupDir, 0755); err != nil {
		fatalError("Не удалось создать папку бекапов %s: %v", LocalBackupDir, err)
	}
	os.MkdirAll(tmpDir, 0755)
	defer os.RemoveAll(tmpDir)

	tmpUfwPath := filepath.Join(tmpDir, "user.rules")
	if err := copyFile(UfwRulesPath, tmpUfwPath); err != nil {
		fmt.Printf("Внимание: Не удалось скопировать UFW rules: %v\n", err)
		os.Create(tmpUfwPath)
	}

	fmt.Println("Архивация Main...")
	argsMain := []string{
		"a", "-m0=lzma2", "-mx=9", "-p" + password, "-mhe=on",
		"-xr!docker/sillytavern", 
		mainArchivePath,
		DockerPath,
		tmpUfwPath,
	}
	if err := run7z(argsMain); err != nil {
		fatalError("Ошибка создания Main архива: %v", err)
	}

	fmt.Println("Архивация SillyTavern...")
	argsSilly := []string{
		"a", "-m0=lzma2", "-mx=9", "-p" + password, "-mhe=on",
		sillyArchivePath,
		SillyPath,
	}
	if err := run7z(argsSilly); err != nil {
		fatalError("Ошибка создания Silly архива: %v", err)
	}

	uploadFile(client, token, mainArchivePath, RemoteBackupDir)
	uploadFile(client, token, sillyArchivePath, RemoteBackupDir)

	sendDiscord(fmt.Sprintf("✅ **Полный цикл бекапа завершен!**\nFiles:\n`%s`\n`%s`", mainArchiveName, sillyArchiveName))
}

func run7z(args []string) error {
	cmd := exec.Command("7z", args...)
	return cmd.Run()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil { return err }
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil { return err }
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// --- ОТПРАВКА В DISCORD ---
func sendDiscord(message string) {
	// Если URL не задан, просто выходим
	if discordWebhookURL == "" {
		return
	}

	payload := DiscordPayload{
		Content:   message,
		Username:  discordBotName,
		AvatarUrl: discordAvatarURL,
	}
	jsonBody, _ := json.Marshal(payload)

	resp, err := http.Post(discordWebhookURL, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		fmt.Printf("Ошибка отправки в Discord: %v\n", err)
		return
	}
	defer resp.Body.Close()
}

// --- ОБРАБОТКА ОШИБОК ---
func fatalError(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println("Ошибка:", msg)
	sendDiscord(fmt.Sprintf("❌ **Критическая ошибка:** %s", msg))
	os.Exit(1)
}

// --- ФУНКЦИЯ ЗАГРУЗКИ ---
func uploadFile(client *http.Client, token, localPath, remoteDir string) {
	file, err := os.Open(localPath)
	if err != nil {
		fatalError("Не удалось открыть файл: %v", err)
	}
	defer file.Close()

	fileName := filepath.Base(localPath)
	destPath := filepath.Join(remoteDir, fileName)

	reqUrl := fmt.Sprintf("%s/upload?path=%s&overwrite=true", baseURL, url.QueryEscape(destPath))
	req, _ := http.NewRequest("GET", reqUrl, nil)
	req.Header.Add("Authorization", "OAuth "+token)

	resp, err := client.Do(req)
	if err != nil {
		fatalError("Ошибка соединения: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 409 {
		fmt.Println("Внимание: Возможно папка назначения не существует. Ошибка 409.")
	}
	if resp.StatusCode != 200 {
		fatalError("Ошибка получения URL загрузки (Code: %d) для %s", resp.StatusCode, fileName)
	}

	var link LinkResponse
	if err := json.NewDecoder(resp.Body).Decode(&link); err != nil {
		fatalError("Ошибка JSON: %v", err)
	}

	stat, _ := file.Stat()
	putReq, _ := http.NewRequest("PUT", link.Href, file)
	putReq.ContentLength = stat.Size()

	fmt.Printf("Загрузка %s (%d bytes)...\n", fileName, stat.Size())
	putResp, err := client.Do(putReq)
	if err != nil {
		fatalError("Сбой передачи: %v", err)
	}

	if putResp.StatusCode != 201 && putResp.StatusCode != 200 {
		fatalError("Файл не принят, код: %d", putResp.StatusCode)
	}

	successMsg := fmt.Sprintf("Файл `%s` загружен!", fileName)
	fmt.Println(successMsg)
}

// --- ФУНКЦИЯ СКАЧИВАНИЯ ---
func downloadNewest(client *http.Client, token, prefix, remoteDir string) {
	reqUrl := fmt.Sprintf("%s?path=%s&sort=-created&limit=100", baseURL, url.QueryEscape(remoteDir))
	req, _ := http.NewRequest("GET", reqUrl, nil)
	req.Header.Add("Authorization", "OAuth "+token)

	resp, err := client.Do(req)
	if err != nil {
		fatalError("Ошибка списка: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fatalError("Ошибка API (Code: %d)", resp.StatusCode)
	}

	var list ResourceList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		fatalError("Ошибка JSON списка: %v", err)
	}

	var targetFile string
	var targetName string
	for _, item := range list.Embedded.Items {
		if item.Type == "file" && strings.HasPrefix(item.Name, prefix) {
			targetFile = item.Path
			targetName = item.Name
			fmt.Printf("Найден файл: %s (от %s)\n", item.Name, item.Created.Format("2006-01-02 15:04"))
			break
		}
	}

	if targetFile == "" {
		fatalError("Файл '%s' не найден", prefix)
	}

	dlUrl := fmt.Sprintf("%s/download?path=%s", baseURL, url.QueryEscape(targetFile))
	dlReq, _ := http.NewRequest("GET", dlUrl, nil)
	dlReq.Header.Add("Authorization", "OAuth "+token)

	dlResp, err := client.Do(dlReq)
	if err != nil {
		fatalError("Ошибка запроса ссылки: %v", err)
	}
	defer dlResp.Body.Close()

	var link LinkResponse
	if err := json.NewDecoder(dlResp.Body).Decode(&link); err != nil {
		fatalError("Ошибка ссылки JSON")
	}

	fileResp, err := http.Get(link.Href)
	if err != nil {
		fatalError("Ошибка скачивания: %v", err)
	}
	defer fileResp.Body.Close()

	outFileName := filepath.Base(targetFile)
	outFile, err := os.Create(outFileName)
	if err != nil {
		fatalError("Ошибка создания файла: %v", err)
	}
	defer outFile.Close()

	_, err = io.Copy(outFile, fileResp.Body)
	if err != nil {
		fatalError("Ошибка записи: %v", err)
	}

	fmt.Printf("Файл '%s' скачан.\n", targetName)
}
