package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	openai "github.com/sashabaranov/go-openai"
)

const (
	ELEVENLABS_VOICE = "3EuKHIEZbSzrHGNmdYsx" // Adam voice (мужской)
	DB_FILE          = "bot_history.db"
)

var db *sql.DB

type ElevenLabsRequest struct {
	Text    string `json:"text"`
	ModelID string `json:"model_id"`
}

type ElevenLabsSTTResponse struct {
	Text string `json:"text"`
}

// initDB инициализирует подключение к базе данных
func initDB() error {
	var err error
	db, err = sql.Open("sqlite3", DB_FILE)
	if err != nil {
		return fmt.Errorf("ошибка открытия БД: %v", err)
	}

	// Проверяем подключение
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ошибка подключения к БД: %v", err)
	}

	// Создаем таблицу мыслей если её нет
	createThoughtsTableSQL := `
	CREATE TABLE IF NOT EXISTS thoughts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		thought_text TEXT NOT NULL,
		category TEXT
	);
	`

	_, err = db.Exec(createThoughtsTableSQL)
	if err != nil {
		return fmt.Errorf("ошибка создания таблицы thoughts: %v", err)
	}

	// Создаем таблицу лимитов пользователей
	createUserLimitsTableSQL := `
	CREATE TABLE IF NOT EXISTS user_limits (
		user_id INTEGER PRIMARY KEY,
		username TEXT,
		date DATE DEFAULT (date('now')),
		request_count INTEGER DEFAULT 0
	);
	`

	_, err = db.Exec(createUserLimitsTableSQL)
	if err != nil {
		return fmt.Errorf("ошибка создания таблицы user_limits: %v", err)
	}

	log.Printf("💾 База данных подключена: %s", DB_FILE)
	log.Printf("✅ Таблица 'thoughts' готова к работе")
	log.Printf("✅ Таблица 'user_limits' готова к работе")
	return nil
}

// saveMessage записывает сообщение в базу данных
func saveMessage(userID int64, username, messageType, inputText, responseType, responseText string) error {
	insertSQL := `
	INSERT INTO messages (timestamp, user_id, username, message_type, input_text, response_type, response_text)
	VALUES (datetime('now'), ?, ?, ?, ?, ?, ?)
	`

	_, err := db.Exec(insertSQL, userID, username, messageType, inputText, responseType, responseText)
	if err != nil {
		return fmt.Errorf("ошибка записи в БД: %v", err)
	}
	log.Printf("💾 Сохранено в БД: user=%s, type=%s", username, messageType)
	return nil
}

// saveThought записывает мысль в базу данных
func saveThought(thoughtText, category string) error {
	insertSQL := `
	INSERT INTO thoughts (timestamp, thought_text, category)
	VALUES (datetime('now'), ?, ?)
	`

	_, err := db.Exec(insertSQL, thoughtText, category)
	if err != nil {
		return fmt.Errorf("ошибка записи мысли в БД: %v", err)
	}
	log.Printf("💭 Мысль сохранена в БД: category=%s", category)
	return nil
}

// checkUserLimit проверяет, не превышен ли дневной лимит пользователя
// Возвращает true если лимит превышен
func checkUserLimit(userID int64, username string) bool {
	// Владелец бота не имеет лимитов
	if username == "roman8890" {
		return false
	}

	const dailyLimit = 2

	// Получаем счетчик запросов за сегодня
	var requestCount int
	var lastDate string

	query := `SELECT request_count, date FROM user_limits WHERE user_id = ?`
	err := db.QueryRow(query, userID).Scan(&requestCount, &lastDate)

	if err == sql.ErrNoRows {
		// Новый пользователь - создаем запись
		insertSQL := `INSERT INTO user_limits (user_id, username, date, request_count) VALUES (?, ?, date('now'), 0)`
		db.Exec(insertSQL, userID, username)
		return false
	}

	if err != nil {
		log.Printf("⚠️ Ошибка проверки лимита: %v", err)
		return false // В случае ошибки разрешаем запрос
	}

	// Проверяем, сегодняшний ли день
	today := strings.Split(lastDate, " ")[0] // Получаем только дату
	currentDate := "" // Получим из БД
	db.QueryRow(`SELECT date('now')`).Scan(&currentDate)

	// Если дата изменилась - сбрасываем счетчик
	if today != currentDate {
		updateSQL := `UPDATE user_limits SET date = date('now'), request_count = 0 WHERE user_id = ?`
		db.Exec(updateSQL, userID)
		return false
	}

	// Проверяем лимит
	if requestCount >= dailyLimit {
		log.Printf("🚫 Пользователь %s (%d) превысил лимит: %d/%d", username, userID, requestCount, dailyLimit)
		return true
	}

	return false
}

// incrementUserUsage увеличивает счетчик использования пользователя
func incrementUserUsage(userID int64, username string) error {
	// Владелец бота не имеет лимитов
	if username == "roman8890" {
		return nil
	}

	updateSQL := `
	UPDATE user_limits
	SET request_count = request_count + 1
	WHERE user_id = ? AND date = date('now')
	`

	result, err := db.Exec(updateSQL, userID)
	if err != nil {
		return fmt.Errorf("ошибка увеличения счетчика: %v", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		// Если записи нет, создаем ее
		insertSQL := `INSERT INTO user_limits (user_id, username, date, request_count) VALUES (?, ?, date('now'), 1)`
		_, err = db.Exec(insertSQL, userID, username)
		if err != nil {
			return fmt.Errorf("ошибка создания записи лимита: %v", err)
		}
	}

	// Получаем текущий счетчик для лога
	var count int
	db.QueryRow(`SELECT request_count FROM user_limits WHERE user_id = ?`, userID).Scan(&count)
	log.Printf("📊 Пользователь %s: запрос %d/2", username, count)

	return nil
}

// generateSQL генерирует SQL запрос из текста пользователя через GPT
func generateSQL(client *openai.Client, userQuery string) (string, error) {
	ctx := context.Background()

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	systemPrompt := `Ты эксперт SQL. Преобразуй запрос пользователя в SQL запрос для SQLite базы данных.

База данных содержит ДВЕ таблицы:

1. Таблица messages (история сообщений):
- id (INTEGER PRIMARY KEY)
- timestamp (DATETIME)
- user_id (INTEGER)
- username (TEXT)
- message_type (TEXT) - тип сообщения: 'text' или 'voice'
- input_text (TEXT) - текст входящего сообщения
- response_type (TEXT) - тип ответа
- response_text (TEXT) - текст ответа

2. Таблица thoughts (мысли/заметки):
- id (INTEGER PRIMARY KEY)
- timestamp (DATETIME)
- thought_text (TEXT) - текст мысли
- category (TEXT) - категория мысли

ВАЖНО:
1. Отвечай ТОЛЬКО SQL запросом, без объяснений
2. Используй SELECT запросы
3. Ограничивай результаты через LIMIT если нужно
4. НЕ используй DELETE, DROP, UPDATE, INSERT
5. По умолчанию НЕ включай в SELECT поля user_id и username (если пользователь явно не спрашивает про пользователей)
6. Используй LIMIT 10 по умолчанию для запросов "покажи записи"

ШАБЛОНЫ ЗАПРОСОВ:

Подсчет:
- "сколько сообщений" → SELECT COUNT(*) as count FROM messages
- "сколько голосовых" → SELECT COUNT(*) as count FROM messages WHERE message_type='voice'
- "сколько текстовых" → SELECT COUNT(*) as count FROM messages WHERE message_type='text'

Последние записи:
- "последние N записей" → SELECT id, timestamp, message_type, input_text, response_text FROM messages ORDER BY timestamp DESC LIMIT N
- "последнее сообщение" → SELECT id, timestamp, message_type, input_text, response_text FROM messages ORDER BY timestamp DESC LIMIT 1

Поиск по содержанию:
- "найди сообщения про [тема]" → SELECT id, timestamp, input_text FROM messages WHERE input_text LIKE '%тема%' LIMIT 10

Статистика по типам:
- "статистика по типам" → SELECT message_type, COUNT(*) as count FROM messages GROUP BY message_type

Временные запросы:
- "сегодняшние сообщения" → SELECT COUNT(*) as count FROM messages WHERE DATE(timestamp) = DATE('now')
- "за последний час" → SELECT COUNT(*) as count FROM messages WHERE timestamp >= datetime('now', '-1 hour')

ЗАПРОСЫ К ТАБЛИЦЕ МЫСЛЕЙ (thoughts):

Подсчет мыслей:
- "сколько мыслей" → SELECT COUNT(*) as count FROM thoughts
- "сколько мыслей по категории [название]" → SELECT COUNT(*) as count FROM thoughts WHERE category='название'

Последние мысли:
- "последние N мыслей" → SELECT id, timestamp, thought_text, category FROM thoughts ORDER BY timestamp DESC LIMIT N
- "последняя мысль" → SELECT id, timestamp, thought_text, category FROM thoughts ORDER BY timestamp DESC LIMIT 1

Поиск мыслей:
- "найди мысли про [тема]" → SELECT id, timestamp, thought_text FROM thoughts WHERE thought_text LIKE '%тема%' LIMIT 10

Мысли по категориям:
- "покажи все категории мыслей" → SELECT DISTINCT category FROM thoughts WHERE category IS NOT NULL
- "мысли категории [название]" → SELECT id, timestamp, thought_text FROM thoughts WHERE category='название' LIMIT 10`

	log.Printf("🔍 Генерирую SQL запрос для: %s", userQuery)

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: systemPrompt,
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: userQuery,
				},
			},
		},
	)

	if err != nil {
		return "", fmt.Errorf("ошибка генерации SQL: %v", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("GPT не вернул SQL запрос")
	}

	sqlQuery := strings.TrimSpace(resp.Choices[0].Message.Content)
	// Убираем markdown форматирование если есть
	sqlQuery = strings.TrimPrefix(sqlQuery, "```sql")
	sqlQuery = strings.TrimPrefix(sqlQuery, "```")
	sqlQuery = strings.TrimSuffix(sqlQuery, "```")
	sqlQuery = strings.TrimSpace(sqlQuery)

	log.Printf("📝 Сгенерирован SQL: %s", sqlQuery)
	return sqlQuery, nil
}

// executeSQL выполняет SQL запрос и возвращает результаты в виде текста
func executeSQL(sqlQuery string) (string, error) {
	// Проверяем, что это SELECT запрос
	upperQuery := strings.ToUpper(strings.TrimSpace(sqlQuery))
	if !strings.HasPrefix(upperQuery, "SELECT") {
		return "", fmt.Errorf("разрешены только SELECT запросы")
	}

	log.Printf("⚡ Выполняю SQL запрос...")

	rows, err := db.Query(sqlQuery)
	if err != nil {
		return "", fmt.Errorf("ошибка выполнения SQL: %v", err)
	}
	defer rows.Close()

	// Получаем названия колонок
	columns, err := rows.Columns()
	if err != nil {
		return "", fmt.Errorf("ошибка получения колонок: %v", err)
	}

	// Формируем результат
	var results []map[string]interface{}

	for rows.Next() {
		// Создаем слайс для сканирования значений
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return "", fmt.Errorf("ошибка сканирования строки: %v", err)
		}

		// Формируем map для строки
		row := make(map[string]interface{})
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = val
			}
		}
		results = append(results, row)
	}

	if len(results) == 0 {
		return "Запрос выполнен успешно, но результатов не найдено.", nil
	}

	// Форматируем результаты в читаемый текст
	resultText := fmt.Sprintf("Найдено записей: %d\n\n", len(results))
	for i, row := range results {
		resultText += fmt.Sprintf("Запись %d:\n", i+1)
		for col, val := range row {
			resultText += fmt.Sprintf("  %s: %v\n", col, val)
		}
		resultText += "\n"
	}

	log.Printf("✅ SQL выполнен успешно, записей: %d", len(results))
	return resultText, nil
}

// formatSQLResults форматирует результаты SQL через GPT для голосового ответа
func formatSQLResults(client *openai.Client, userQuery, sqlResults string) (string, error) {
	ctx := context.Background()

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	systemPrompt := `Ты голосовой помощник. Преобразуй результаты SQL запроса в краткий, понятный голосовой ответ на русском языке.

ВАЖНО:
1. Ответ должен быть КОРОТКИМ (до 30 слов)
2. Говори по-человечески, как будто объясняешь другу
3. Не упоминай технические детали (SQL, базы данных)
4. Если результатов много, обобщи информацию`

	prompt := fmt.Sprintf("Вопрос пользователя: %s\n\nРезультаты из базы данных:\n%s", userQuery, sqlResults)

	log.Printf("💬 Форматирую ответ для пользователя...")

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: systemPrompt,
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: prompt,
				},
			},
		},
	)

	if err != nil {
		return "", fmt.Errorf("ошибка форматирования ответа: %v", err)
	}

	if len(resp.Choices) == 0 {
		return "Не удалось сформировать ответ.", nil
	}

	answer := strings.TrimSpace(resp.Choices[0].Message.Content)
	log.Printf("✅ Ответ сформирован: %s", answer)
	return answer, nil
}

// getChatGPTResponse отправляет запрос к ChatGPT и возвращает ответ
func getChatGPTResponse(client *openai.Client, userMessage string) (string, error) {
	ctx := context.Background()

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}

	log.Printf("🤖 Отправляю запрос в ChatGPT (модель: %s)", model)
	log.Printf("📝 Сообщение пользователя: %s", userMessage)

	resp, err := client.CreateChatCompletion(
		ctx,
		openai.ChatCompletionRequest{
			Model: model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleSystem,
					Content: "Ты эксперт IT Go Backend, отвечай коротко и по делу. Меньше 20 слов в ответе.",
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: userMessage,
				},
			},
		},
	)

	if err != nil {
		log.Printf("❌ Ошибка от ChatGPT API: %v", err)
		return "", fmt.Errorf("ошибка ChatGPT: %v", err)
	}

	if len(resp.Choices) == 0 {
		log.Printf("⚠️  ChatGPT вернул пустой ответ")
		return "Извините, не удалось получить ответ.", nil
	}

	answer := resp.Choices[0].Message.Content
	log.Printf("✅ Получен ответ от ChatGPT (длина: %d символов)", len(answer))

	return answer, nil
}

// speechToText преобразует аудиофайл в текст с помощью ElevenLabs STT
func speechToText(audioPath string) (string, error) {
	// Открываем аудио файл
	file, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("ошибка открытия файла: %v", err)
	}
	defer file.Close()

	// Создаем multipart form
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Добавляем файл
	part, err := writer.CreateFormFile("file", "audio.ogg")
	if err != nil {
		return "", fmt.Errorf("ошибка создания form file: %v", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", fmt.Errorf("ошибка копирования файла: %v", err)
	}

	// Добавляем модель для STT
	writer.WriteField("model_id", "scribe_v2") // Самая новая модель для распознавания

	writer.Close()

	// Отправляем запрос к ElevenLabs Speech-to-Text API
	url := "https://api.elevenlabs.io/v1/speech-to-text"
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("xi-api-key", os.Getenv("ELEVENLABS_API_KEY"))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка отправки запроса: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ошибка API (статус %d): %s", resp.StatusCode, string(bodyBytes))
	}

	// Парсим ответ
	var result ElevenLabsSTTResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("ошибка декодирования ответа: %v", err)
	}

	return result.Text, nil
}

// Функция для преобразования текста в голос через ElevenLabs
func textToSpeech(text string) ([]byte, error) {
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", ELEVENLABS_VOICE)

	requestBody := ElevenLabsRequest{
		Text:    text,
		ModelID: "eleven_multilingual_v2", // Поддержка русского языка
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания JSON: %v", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", os.Getenv("ELEVENLABS_API_KEY"))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка отправки запроса: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ошибка API (статус %d): %s", resp.StatusCode, string(body))
	}

	audioData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения аудио: %v", err)
	}

	return audioData, nil
}

func main() {
	// Загружаем переменные окружения из .env файла
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Ошибка загрузки .env файла")
	}

	// Инициализируем базу данных
	if err := initDB(); err != nil {
		log.Fatalf("❌ Ошибка инициализации БД: %v", err)
	}
	defer db.Close()

	// Проверяем наличие ключей
	openaiKey := os.Getenv("OPENAI_API_KEY")
	if openaiKey == "" {
		log.Fatal("❌ OPENAI_API_KEY не найден в .env файле")
	}
	log.Printf("✅ OpenAI API ключ загружен (длина: %d)", len(openaiKey))

	elevenlabsKey := os.Getenv("ELEVENLABS_API_KEY")
	if elevenlabsKey == "" {
		log.Fatal("❌ ELEVENLABS_API_KEY не найден в .env файле")
	}
	log.Printf("✅ ElevenLabs API ключ загружен")

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
		log.Printf("⚠️  OPENAI_MODEL не задан, используется по умолчанию: %s", model)
	} else {
		log.Printf("✅ Модель OpenAI: %s", model)
	}

	// Создаем экземпляр бота
	bot, err := tgbotapi.NewBotAPI(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if err != nil {
		log.Panic(err)
	}

	// Создаем клиент OpenAI
	openaiClient := openai.NewClient(openaiKey)

	bot.Debug = false

	log.Printf("🤖 Авторизован как %s", bot.Self.UserName)
	log.Printf("🎙️ Бот с голосовыми сообщениями и ChatGPT запущен!")

	// Настройка получения обновлений
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	// Обработка входящих сообщений
	for update := range updates {
		if update.Message == nil {
			continue
		}

		// Проверяем лимит запросов (кроме команд /start и /help)
		if !update.Message.IsCommand() || (update.Message.IsCommand() && update.Message.Command() != "start" && update.Message.Command() != "help") {
			username := update.Message.From.UserName
			userID := update.Message.From.ID

			// Проверяем лимит
			if checkUserLimit(userID, username) {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"⏳ Вы достигли дневного лимита запросов (2 запроса в день).\n\n"+
						"Лимит обновляется каждый день в 00:00 UTC.\n"+
						"Спасибо за понимание! 🙏")
				bot.Send(msg)
				log.Printf("🚫 Запрос от %s отклонен - лимит превышен", username)
				continue
			}

			// Увеличиваем счетчик использования
			incrementUserUsage(userID, username)
		}

		// Обработка голосовых сообщений
		if update.Message.Voice != nil {
			log.Printf("🎤 [%s] Получено голосовое сообщение", update.Message.From.UserName)

			// Отправляем уведомление
			statusMsg := tgbotapi.NewMessage(update.Message.Chat.ID, "🎧 Распознаю голос...")
			bot.Send(statusMsg)

			// Получаем информацию о файле
			fileConfig := tgbotapi.FileConfig{FileID: update.Message.Voice.FileID}
			file, err := bot.GetFile(fileConfig)
			if err != nil {
				log.Printf("Ошибка получения файла: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID, "❌ Ошибка получения голосового файла")
				bot.Send(msg)
				continue
			}

			// Скачиваем файл
			fileURL := file.Link(os.Getenv("TELEGRAM_BOT_TOKEN"))
			resp, err := http.Get(fileURL)
			if err != nil {
				log.Printf("Ошибка скачивания: %v", err)
				continue
			}
			defer resp.Body.Close()

			// Сохраняем во временный файл
			tmpFile, err := os.CreateTemp("", "voice-*.ogg")
			if err != nil {
				log.Printf("Ошибка создания файла: %v", err)
				continue
			}
			tmpFileName := tmpFile.Name()
			defer os.Remove(tmpFileName)

			if _, err := io.Copy(tmpFile, resp.Body); err != nil {
				log.Printf("Ошибка сохранения: %v", err)
				tmpFile.Close()
				continue
			}
			tmpFile.Close()

			// Распознаем голос через ElevenLabs STT
			recognizedText, err := speechToText(tmpFileName)
			if err != nil {
				log.Printf("Ошибка распознавания: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					fmt.Sprintf("❌ Ошибка распознавания: %v", err))
				bot.Send(msg)
				continue
			}

			log.Printf("📝 Распознано: %s", recognizedText)

			var gptResponse string

			// Проверяем ключевые слова
			lowerText := strings.ToLower(strings.TrimSpace(recognizedText))
			if strings.HasPrefix(lowerText, "мысль") {
				// Проверяем права доступа
				if update.Message.From.UserName != "roman8890" {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"❌ У вас нет доступа к этой функции")
					bot.Send(msg)
					log.Printf("🚫 Попытка сохранить мысль от пользователя %s", update.Message.From.UserName)
					continue
				}

				// Убираем слово "мысль" из текста
				thoughtText := strings.TrimSpace(strings.TrimPrefix(lowerText, "мысль"))

				if thoughtText == "" {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"❌ Укажите текст мысли после слова 'мысль'")
					bot.Send(msg)
					continue
				}

				log.Printf("💭 Сохраняю мысль: %s", thoughtText)

				// Сохраняем мысль в БД
				err := saveThought(thoughtText, "general")
				if err != nil {
					log.Printf("Ошибка сохранения мысли: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка сохранения: %v", err))
					bot.Send(msg)
					continue
				}

				// Озвучиваем подтверждение
				gptResponse = "Мысль сохранена"

			} else if strings.HasPrefix(lowerText, "база") {
				// Убираем слово "база" из запроса
				userQuery := strings.TrimSpace(strings.TrimPrefix(lowerText, "база"))

				log.Printf("💾 Обработка запроса к базе данных: %s", userQuery)

				// Отправляем уведомление
				statusMsg2 := tgbotapi.NewMessage(update.Message.Chat.ID,
					"💾 Обрабатываю запрос к базе данных...")
				bot.Send(statusMsg2)

				// 1. Генерируем SQL запрос через GPT
				sqlQuery, err := generateSQL(openaiClient, userQuery)
				if err != nil {
					log.Printf("Ошибка генерации SQL: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка генерации SQL: %v", err))
					bot.Send(msg)
					continue
				}

				// 2. Выполняем SQL запрос
				sqlResults, err := executeSQL(sqlQuery)
				if err != nil {
					log.Printf("Ошибка выполнения SQL: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка выполнения запроса: %v", err))
					bot.Send(msg)
					continue
				}

				// 3. Форматируем результаты через GPT
				gptResponse, err = formatSQLResults(openaiClient, userQuery, sqlResults)
				if err != nil {
					log.Printf("Ошибка форматирования ответа: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка форматирования: %v", err))
					bot.Send(msg)
					continue
				}
			} else {
				// Обычный режим - отправляем вопрос в ChatGPT
				statusMsg2 := tgbotapi.NewMessage(update.Message.Chat.ID,
					fmt.Sprintf("🤖 Вы сказали: \"%s\"\n\nДумаю над ответом...", recognizedText))
				bot.Send(statusMsg2)

				gptResponse, err = getChatGPTResponse(openaiClient, recognizedText)
				if err != nil {
					log.Printf("Ошибка ChatGPT: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка получения ответа от ChatGPT: %v", err))
					bot.Send(msg)
					continue
				}
			}

			log.Printf("💬 GPT ответ: %s", gptResponse)

			// Ограничение длины для озвучивания
			if len(gptResponse) > 500 {
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					fmt.Sprintf("📝 Ответ:\n%s\n\n⚠️ Ответ слишком длинный для озвучивания", gptResponse))
				bot.Send(msg)
				continue
			}

			// Отправляем уведомление о генерации голоса
			statusMsg3 := tgbotapi.NewMessage(update.Message.Chat.ID, "🎤 Генерирую голосовое сообщение...")
			bot.Send(statusMsg3)

			// Преобразуем ответ в голос через ElevenLabs TTS
			audioData, err := textToSpeech(gptResponse)
			if err != nil {
				log.Printf("Ошибка TTS: %v", err)
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					fmt.Sprintf("📝 %s\n\n❌ Ошибка генерации голоса: %v", gptResponse, err))
				bot.Send(msg)
				continue
			}

			// Сохраняем и отправляем голосовое сообщение
			tmpFile2, err := os.CreateTemp("", "voice-response-*.mp3")
			if err != nil {
				log.Printf("Ошибка создания файла: %v", err)
				continue
			}
			defer os.Remove(tmpFile2.Name())

			tmpFile2.Write(audioData)
			tmpFile2.Close()

			voice := tgbotapi.NewVoice(update.Message.Chat.ID, tgbotapi.FilePath(tmpFile2.Name()))
			voice.Caption = fmt.Sprintf("🔊 %s", gptResponse)
			if _, err := bot.Send(voice); err != nil {
				log.Printf("Ошибка отправки голоса: %v", err)
			} else {
				// Сохраняем в БД только при успешной отправке
				saveMessage(
					update.Message.From.ID,
					update.Message.From.UserName,
					"voice",
					recognizedText,
					"voice",
					gptResponse,
				)
			}

			log.Printf("✅ Голосовое сообщение успешно обработано")
			continue
		}

		// Обработка команд
		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"🎙️ Привет! Я голосовой бот с ChatGPT и ElevenLabs!\n\n"+
						"✨ Что я умею:\n"+
						"🎤 Голос → GPT → Голос\n"+
						"📝 Текст → GPT → Голос\n"+
						"🔊 /voice [текст] → Голос\n\n"+
						"Команды:\n"+
						"/voice [текст] - просто озвучить текст\n"+
						"/help - помощь\n\n"+
						"💡 Попробуйте задать любой вопрос!\n\n"+
						"Пишите или говорите - я отвечу голосом! 🤖🔊")
				bot.Send(msg)

			case "help":
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"🎙️ Как я работаю:\n\n"+
						"1️⃣ 🎤 ГОЛОСОВОЕ сообщение:\n"+
						"   → ElevenLabs STT → ChatGPT → ElevenLabs TTS\n\n"+
						"2️⃣ 📝 ТЕКСТ:\n"+
						"   → ChatGPT → ElevenLabs TTS\n\n"+
						"3️⃣ 🔊 /voice [текст]:\n"+
						"   → Просто озвучивает текст\n\n"+
						"Технологии:\n"+
						"🤖 ChatGPT (gpt-4o-mini)\n"+
						"🎤 ElevenLabs STT (scribe_v2)\n"+
						"🔊 ElevenLabs TTS (multilingual_v2)\n\n"+
						"⏳ Лимит: 2 запроса в день")
				bot.Send(msg)

			case "voice":
				// Получаем текст после команды
				text := strings.TrimSpace(strings.TrimPrefix(update.Message.Text, "/voice"))
				if text == "" {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"Укажите текст после команды:\n/voice Ваш текст здесь")
					bot.Send(msg)
					continue
				}

				// Отправляем уведомление о генерации
				statusMsg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"🎤 Генерирую голосовое сообщение...")
				bot.Send(statusMsg)

				// Преобразуем текст в голос
				audioData, err := textToSpeech(text)
				if err != nil {
					log.Printf("Ошибка ElevenLabs: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка генерации голоса: %v", err))
					bot.Send(msg)
					continue
				}

				// Сохраняем во временный файл
				tmpFile, err := os.CreateTemp("", "voice-*.mp3")
				if err != nil {
					log.Printf("Ошибка создания файла: %v", err)
					continue
				}
				defer os.Remove(tmpFile.Name())

				if _, err := tmpFile.Write(audioData); err != nil {
					log.Printf("Ошибка записи файла: %v", err)
					tmpFile.Close()
					continue
				}
				tmpFile.Close()

				// Отправляем голосовое сообщение
				voice := tgbotapi.NewVoice(update.Message.Chat.ID, tgbotapi.FilePath(tmpFile.Name()))
				voice.Caption = fmt.Sprintf("🔊 %s", text)
				if _, err := bot.Send(voice); err != nil {
					log.Printf("Ошибка отправки голоса: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"❌ Ошибка отправки голосового сообщения")
					bot.Send(msg)
				}

			default:
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"Неизвестная команда. Используйте /help")
				bot.Send(msg)
			}
		} else if update.Message.Text != "" {
			// Обычные текстовые сообщения
			userText := update.Message.Text
			log.Printf("[%s] %s", update.Message.From.UserName, userText)

			var gptResponse string

			// Проверяем ключевые слова
			lowerText := strings.ToLower(strings.TrimSpace(userText))
			if strings.HasPrefix(lowerText, "мысль") {
				// Проверяем права доступа
				if update.Message.From.UserName != "roman8890" {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"❌ У вас нет доступа к этой функции")
					bot.Send(msg)
					log.Printf("🚫 Попытка сохранить мысль от пользователя %s", update.Message.From.UserName)
					continue
				}

				// Убираем слово "мысль" из текста
				thoughtText := strings.TrimSpace(strings.TrimPrefix(lowerText, "мысль"))

				if thoughtText == "" {
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						"❌ Укажите текст мысли после слова 'мысль'")
					bot.Send(msg)
					continue
				}

				log.Printf("💭 Сохраняю мысль: %s", thoughtText)

				// Сохраняем мысль в БД
				err := saveThought(thoughtText, "general")
				if err != nil {
					log.Printf("Ошибка сохранения мысли: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка сохранения: %v", err))
					bot.Send(msg)
					continue
				}

				// Озвучиваем подтверждение
				gptResponse = "Мысль сохранена"

			} else if strings.HasPrefix(lowerText, "база") {
				// Убираем слово "база" из запроса
				userQuery := strings.TrimSpace(strings.TrimPrefix(lowerText, "база"))

				log.Printf("💾 Обработка запроса к базе данных: %s", userQuery)

				// Отправляем уведомление
				statusMsg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"💾 Обрабатываю запрос к базе данных...")
				bot.Send(statusMsg)

				// 1. Генерируем SQL запрос через GPT
				sqlQuery, err := generateSQL(openaiClient, userQuery)
				if err != nil {
					log.Printf("Ошибка генерации SQL: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка генерации SQL: %v", err))
					bot.Send(msg)
					continue
				}

				// 2. Выполняем SQL запрос
				sqlResults, err := executeSQL(sqlQuery)
				if err != nil {
					log.Printf("Ошибка выполнения SQL: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка выполнения запроса: %v", err))
					bot.Send(msg)
					continue
				}

				// 3. Форматируем результаты через GPT
				gptResponse, err = formatSQLResults(openaiClient, userQuery, sqlResults)
				if err != nil {
					log.Printf("Ошибка форматирования ответа: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка форматирования: %v", err))
					bot.Send(msg)
					continue
				}
			} else {
				// Обычный режим - получаем ответ от ChatGPT
				statusMsg := tgbotapi.NewMessage(update.Message.Chat.ID,
					"🤖 Думаю над ответом...")
				bot.Send(statusMsg)

				gptResponse, err = getChatGPTResponse(openaiClient, userText)
				if err != nil {
					log.Printf("Ошибка ChatGPT: %v", err)
					msg := tgbotapi.NewMessage(update.Message.Chat.ID,
						fmt.Sprintf("❌ Ошибка получения ответа от ChatGPT: %v", err))
					bot.Send(msg)
					continue
				}
			}

			log.Printf("💬 GPT ответ: %s", gptResponse)

			// Ограничение длины текста для озвучивания
			if len(gptResponse) > 500 {
				// Отправляем текстовый ответ, если слишком длинный
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					fmt.Sprintf("📝 %s\n\n⚠️ Ответ слишком длинный для озвучивания (макс. 500 символов)", gptResponse))
				bot.Send(msg)
				continue
			}

			// Генерируем голос
			statusMsg2 := tgbotapi.NewMessage(update.Message.Chat.ID,
				"🎤 Генерирую голосовое сообщение...")
			bot.Send(statusMsg2)

			audioData, err := textToSpeech(gptResponse)
			if err != nil {
				log.Printf("Ошибка ElevenLabs: %v", err)
				// Отправляем хотя бы текстовый ответ
				msg := tgbotapi.NewMessage(update.Message.Chat.ID,
					fmt.Sprintf("📝 %s\n\n❌ Ошибка генерации голоса: %v", gptResponse, err))
				bot.Send(msg)
				continue
			}

			// Сохраняем и отправляем
			tmpFile, err := os.CreateTemp("", "voice-*.mp3")
			if err != nil {
				log.Printf("Ошибка создания файла: %v", err)
				continue
			}
			defer os.Remove(tmpFile.Name())

			tmpFile.Write(audioData)
			tmpFile.Close()

			voice := tgbotapi.NewVoice(update.Message.Chat.ID, tgbotapi.FilePath(tmpFile.Name()))
			voice.Caption = fmt.Sprintf("🔊 %s", gptResponse)
			if _, err := bot.Send(voice); err != nil {
				log.Printf("Ошибка отправки голоса: %v", err)
			} else {
				// Сохраняем в БД только при успешной отправке
				saveMessage(
					update.Message.From.ID,
					update.Message.From.UserName,
					"text",
					userText,
					"voice",
					gptResponse,
				)
			}
		}
	}
}
