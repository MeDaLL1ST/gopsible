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
	Name     string `yaml:"name"`
	Address  string `yaml:"address"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	KeyPath  string `yaml:"key_path"`
}

func (h HostConfig) ID() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Address
}

type Task struct {
	Name         string `yaml:"name"`
	Type         string `yaml:"type"` // script, upload, etc.
	IgnoreErrors bool   `yaml:"ignore_errors"`

	Script string `yaml:"script"`
	Src    string `yaml:"src"`
	Dest   string `yaml:"dest"`
	Mode   string `yaml:"mode"`
}

type Playbook struct {
	Settings Settings               `yaml:"settings"`
	Vars     map[string]interface{} `yaml:"vars"`
	Hosts    []HostConfig           `yaml:"hosts"`
	Tasks    []Task                 `yaml:"tasks"`
}

// Интерфейс, который должен реализовать любой модуль
type Module interface {
	Execute(ctx context.Context, client *ssh.Client, task Task, vars map[string]interface{}) error
}

var modules = map[string]Module{
	"script": &ScriptModule{},
	"upload": &UploadModule{},
	// Сюда добавить новые: "git": &GitModule{}, "docker": &DockerModule{}
}

type ScriptModule struct{}

func (m *ScriptModule) Execute(ctx context.Context, client *ssh.Client, task Task, vars map[string]interface{}) error {
	scriptCmd := renderTemplate(task.Script, vars)

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var stderr bytes.Buffer
	session.Stderr = &stderr

	cmd := fmt.Sprintf("bash -e -c '%s'", strings.ReplaceAll(scriptCmd, "'", "'\\''"))

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("%v | STDERR: %s", err, stderr.String())
	}

	return nil
}

type UploadModule struct{}

func (m *UploadModule) Execute(ctx context.Context, client *ssh.Client, task Task, vars map[string]interface{}) error {
	src := renderTemplate(task.Src, vars)
	dest := renderTemplate(task.Dest, vars)

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("ошибка SFTP: %v", err)
	}
	defer sftpClient.Close()

	fSrc, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("нет локального файла: %v", err)
	}
	defer fSrc.Close()

	fDest, err := sftpClient.Create(dest)
	if err != nil {
		return fmt.Errorf("не удалось создать файл на сервере: %v", err)
	}
	defer fDest.Close()

	if _, err := io.Copy(fDest, fSrc); err != nil {
		return err
	}

	if task.Mode != "" {
		mode, _ := strconv.ParseUint(task.Mode, 8, 32)
		sftpClient.Chmod(dest, os.FileMode(mode))
	}

	fmt.Printf("    📂 Загружено: %s -> %s\n", src, dest)
	return nil
}

func main() {
	playbookFiles := os.Args[1:]
	if len(playbookFiles) == 0 {
		playbookFiles = []string{"playbook.yaml"}
	}

	for _, file := range playbookFiles {
		fmt.Printf("📖 Запуск плейбука: %s\n", file)
		if err := runPlaybook(file); err != nil {
			log.Fatalf("⛔ Фатальная ошибка: %v", err)
		}
	}
	fmt.Println("\n✨ Все задачи выполнены успешно!")
}

func runPlaybook(filename string) error {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	var pb Playbook
	if err := yaml.Unmarshal(data, &pb); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errChan := make(chan error, len(pb.Hosts))

	for _, host := range pb.Hosts {
		wg.Add(1)
		go func(h HostConfig) {
			defer wg.Done()
			if err := runHost(ctx, h, pb); err != nil {
				fmt.Printf("❌ [%s] Ошибка: %v\n", h.ID(), err)
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
		return fmt.Errorf("плейбук завершен с ошибками")
	}
	return nil
}

func runHost(ctx context.Context, host HostConfig, pb Playbook) error {
	sshConfig, err := getSSHConfig(host)
	if err != nil {
		return err
	}

	client, err := ssh.Dial("tcp", host.Address, sshConfig)
	if err != nil {
		return fmt.Errorf("connection failed: %v", err)
	}
	defer client.Close()

	fmt.Printf("🔗 [%s] Подключено (%s)\n", host.ID(), host.Address)

	for _, task := range pb.Tasks {
		select {
		case <-ctx.Done():
			return fmt.Errorf("прервано")
		default:
		}

		taskName := renderTemplate(task.Name, pb.Vars)

		// Поиск модуля
		moduleType := task.Type
		if moduleType == "" {
			moduleType = "script" // дефолт
		}

		handler, exists := modules[moduleType]
		if !exists {
			return fmt.Errorf("неизвестный тип задачи: %s", moduleType)
		}

		// Выполнение модуля
		err := handler.Execute(ctx, client, task, pb.Vars)

		if err != nil {
			if task.IgnoreErrors {
				fmt.Printf("⚠️  [%s] %s (игнорируется): %v\n", host.ID(), taskName, err)
			} else {
				return fmt.Errorf("задача '%s' провалена: %v", taskName, err)
			}
		} else {
			fmt.Printf("✅ [%s] %s\n", host.ID(), taskName)
		}
	}
	return nil
}

func renderTemplate(tmplStr string, vars map[string]interface{}) string {
	t, err := template.New("t").Parse(tmplStr)
	if err != nil {
		return tmplStr
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return tmplStr
	}
	return buf.String()
}

func getSSHConfig(host HostConfig) (*ssh.ClientConfig, error) {
	var auth []ssh.AuthMethod
	if host.Password != "" {
		auth = append(auth, ssh.Password(host.Password))
	}
	if host.KeyPath != "" {
		key, err := ioutil.ReadFile(host.KeyPath)
		if err == nil {
			signer, err := ssh.ParsePrivateKey(key)
			if err == nil {
				auth = append(auth, ssh.PublicKeys(signer))
			}
		}
	}

	if len(auth) == 0 {
		return nil, fmt.Errorf("нет учетных данных (password/key)")
	}

	return &ssh.ClientConfig{
		User:            host.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}
