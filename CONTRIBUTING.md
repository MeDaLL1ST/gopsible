### 🧩 Как добавлять новые модули (Инструкция для разработчиков)
Пример: Добавляем модуль ping (проверка связи без ssh, просто echo)
1. Создаем структуру:
```go
type PingModule struct{}
```
2. Реализуем метод Execute:
```go
func (m *PingModule) Execute(ctx context.Context, client *ssh.Client, task Task, vars map[string]interface{}) error {
    fmt.Println("    🏓 Pong!")
    return nil
}
```
3. Регистрируем в переменной modules:
```go
var modules = map[string]Module{
    "script": &ScriptModule{},
    "upload": &UploadModule{},
    "ping":   &PingModule{}, // <--- Добавили
}
```
Теперь в YAML можно писать type: ping.