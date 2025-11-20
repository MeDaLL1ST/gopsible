package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

type Settings struct {
	FailFast bool `yaml:"fail_fast"`
}

type HostConfig struct {
	Address  string `yaml:"address"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	KeyPath  string `yaml:"key_path"`
}

type Task struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // "script" (default) или "upload"

	// Для type: script
	Script string `yaml:"script"`

	// Для type: upload
	Src  string `yaml:"src"`
	Dest string `yaml:"dest"`
	Mode string `yaml:"mode"` // Например "0755"

	IgnoreErrors bool `yaml:"ignore_errors"`
}

type Playbook struct {
	Settings Settings               `yaml:"settings"`
	Vars     map[string]interface{} `yaml:"vars"`
	Hosts    []HostConfig           `yaml:"hosts"`
	Tasks    []Task                 `yaml:"tasks"`
}

func main() {
	// 1. Обработка аргументов командной строки
	playbookFiles := os.Args[1:]

	if len(playbookFiles) == 0 {
		// По дефолту
		playbookFiles = []string{"playbook.yaml"}
	}

	fmt.Printf("📦 Будут выполнены плейбуки: %v\n", playbookFiles)

	// 2. Последовательный запуск плейбуков
	for _, file := range playbookFiles {
		fmt.Printf("\n>>> ЗАПУСК ПЛЕЙБУКА: %s <<<\n", file)
		if err := runPlaybook(file); err != nil {
			log.Fatalf("⛔ Ошибка выполнения плейбука '%s': %v", file, err)
		}
	}

	fmt.Println("\n🎉 Все плейбуки успешно выполнены!")
}

func runPlaybook(filename string) error {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("не удалось прочитать файл: %v", err)
	}

	var pb Playbook
	if err := yaml.Unmarshal(data, &pb); err != nil {
		return fmt.Errorf("ошибка YAML: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errChan := make(chan error, len(pb.Hosts))

	// Запускаем хосты параллельно
	for _, host := range pb.Hosts {
		wg.Add(1)
		go func(h HostConfig) {
			defer wg.Done()
			if err := runHost(ctx, h, pb); err != nil {
				fmt.Printf("❌ [%s] ОШИБКА: %v\n", h.Address, err)
				errChan <- err
				if pb.Settings.FailFast {
					cancel()
				}
			}
		}(host)
	}

	wg.Wait()
	close(errChan)

	if len(errChan) > 0 {
		return fmt.Errorf("были ошибки на хостах")
	}
	return nil
}

func runHost(ctx context.Context, host HostConfig, pb Playbook) error {
	// --- Аутентификация ---
	config, err := getSSHConfig(host)
	if err != nil {
		return err
	}

	client, err := ssh.Dial("tcp", host.Address, config)
	if err != nil {
		return fmt.Errorf("ошибка подключения: %v", err)
	}
	defer client.Close()

	fmt.Printf("🔗 [%s] Подключено\n", host.Address)

	var sftpClient *sftp.Client
	sftpClient, err = sftp.NewClient(client)
	if err == nil {
		defer sftpClient.Close()
	}

	// --- Выполнение задач ---
	for _, task := range pb.Tasks {
		select {
		case <-ctx.Done():
			return fmt.Errorf("прервано")
		default:
		}

		// Шаблонизация полей задачи
		taskName := renderTemplate(task.Name, pb.Vars)

		// Логика выбора типа задачи
		switch task.Type {
		case "upload":
			// Шаблонизируем пути
			src := renderTemplate(task.Src, pb.Vars)
			dest := renderTemplate(task.Dest, pb.Vars)

			err = uploadFile(sftpClient, src, dest, task.Mode)
			if err != nil && !task.IgnoreErrors {
				return fmt.Errorf("задача '%s' (upload) провалена: %v", taskName, err)
			}
			fmt.Printf("📂 [%s] Uploaded: %s -> %s\n", host.Address, src, dest)

		case "script", "": // script или пусто - это выполнение команды
			scriptRaw := renderTemplate(task.Script, pb.Vars)

			err = runCommand(client, scriptRaw)
			if err != nil && !task.IgnoreErrors {
				return fmt.Errorf("задача '%s' провалена: %v", taskName, err)
			}
			fmt.Printf("✅ [%s] %s\n", host.Address, taskName)

		default:
			return fmt.Errorf("неизвестный тип задачи: %s", task.Type)
		}
	}
	return nil
}

// --- Вспомогательные функции ---

// Выполнение Bash скрипта
func runCommand(client *ssh.Client, script string) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var stderr bytes.Buffer
	session.Stderr = &stderr

	// Добавляем bash -e чтобы падать при ошибках
	cmd := fmt.Sprintf("bash -e -c '%s'", strings.ReplaceAll(script, "'", "'\\''"))

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("%v | STDERR: %s", err, stderr.String())
	}
	return nil
}

// Загрузка файла через SFTP
func uploadFile(client *sftp.Client, localPath, remotePath string, modeStr string) error {
	if client == nil {
		return fmt.Errorf("SFTP клиент не инициализирован")
	}

	// Открываем локальный файл
	srcFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("не найден локальный файл: %v", err)
	}
	defer srcFile.Close()

	dstFile, err := client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("не удалось создать удаленный файл: %v", err)
	}
	defer dstFile.Close()

	// Копируем данные
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("ошибка передачи данных: %v", err)
	}

	if modeStr != "" {
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err == nil {
			if err := client.Chmod(remotePath, os.FileMode(mode)); err != nil {
				return fmt.Errorf("ошибка chmod: %v", err)
			}
		}
	}

	return nil
}

func renderTemplate(tmplStr string, vars map[string]interface{}) string {
	t, err := template.New("t").Parse(tmplStr)
	if err != nil {
		return tmplStr // Возвращаем как есть при ошибке, или можно паниковать
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return tmplStr
	}
	return buf.String()
}

// Настройка SSH
func getSSHConfig(host HostConfig) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod
	if host.Password != "" {
		authMethods = append(authMethods, ssh.Password(host.Password))
	}
	if host.KeyPath != "" {
		key, err := ioutil.ReadFile(host.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("ошибка чтения ключа: %v", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ошибка парсинга ключа: %v", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("нет кредов для %s", host.Address)
	}

	return &ssh.ClientConfig{
		User:            host.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}
