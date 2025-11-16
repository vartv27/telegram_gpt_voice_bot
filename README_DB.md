# База данных бота

## Описание

Бот использует **SQLite** для хранения всех запросов и ответов.

## Структура БД

**Файл**: `bot_history.db`

**Таблица**: `messages`

| Поле | Тип | Описание |
|------|-----|----------|
| `id` | INTEGER | Автоинкремент, первичный ключ |
| `timestamp` | DATETIME | Время сообщения |
| `user_id` | INTEGER | ID пользователя Telegram |
| `username` | TEXT | Username пользователя |
| `message_type` | TEXT | Тип входящего сообщения: `text` или `voice` |
| `input_text` | TEXT | Текст запроса (или распознанный текст из голоса) |
| `response_type` | TEXT | Тип ответа: `text` или `voice` |
| `response_text` | TEXT | Текст ответа |

## Примеры использования

### Получить все сообщения
```sql
SELECT * FROM messages ORDER BY timestamp DESC;
```

### Статистика по типам сообщений
```sql
SELECT
    message_type,
    COUNT(*) as count
FROM messages
GROUP BY message_type;
```

### Сообщения конкретного пользователя
```sql
SELECT * FROM messages
WHERE username = 'roman8890'
ORDER BY timestamp DESC;
```

### Последние 10 сообщений
```sql
SELECT
    timestamp,
    username,
    message_type,
    input_text
FROM messages
ORDER BY timestamp DESC
LIMIT 10;
```

## Команды бота

- `/stats` - показывает статистику из БД

## Просмотр БД

Используйте любой SQLite клиент:

```bash
# Через командную строку
sqlite3 bot_history.db

# Примеры запросов
sqlite> SELECT COUNT(*) FROM messages;
sqlite> SELECT * FROM messages LIMIT 5;
sqlite> .quit
```

## Безопасность

- Файл `bot_history.db` добавлен в `.gitignore`
- БД не будет отправляться в Git репозиторий
- Все личные данные остаются локально
