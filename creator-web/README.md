# Creator Web Panel

Web-интерфейс для управления headless VPN-туннелями через видеоплатформы (VK, Telemost, WB Stream, DION).  
Предназначен для развёртывания на VDS. Заменяет Electron `creator-app` для серверного использования.

## Возможности

- 🔗 **Управление подключениями** — создание, остановка, удаление через браузер
- 💾 **Персистенция** — подключения сохраняются в JSON и восстанавливаются при перезапуске
- 🏷️ **Алиасы** — человекочитаемые имена для каждого подключения
- 🍪 **Загрузка cookies** — upload JSON-файлов куки через web-интерфейс
- 📡 **Real-time логи** — WebSocket стриминг логов от headless-процессов
- 📱 **QR-коды** — для подключения мобильных клиентов
- 🔒 **HTTP Basic Auth** — защита панели логином/паролем
- 🐳 **Docker** — готовый образ со всеми бинарниками

---

## Быстрый старт с Docker (рекомендуется)

### 1. Клонировать репозиторий

```bash
git clone https://github.com/vpaull11/whitelist-bypass.git
cd whitelist-bypass
```

### 2. Настроить переменные окружения

```bash
cd creator-web
cp .env.example .env
nano .env
```

Обязательно измените:
```env
WEB_PORT=8080           # Порт веб-интерфейса (любой свободный)
WEB_USER=admin          # Логин для входа
WEB_PASS=your-password  # Пароль для входа
```

### 3. Собрать и запустить

```bash
docker compose up -d --build
```

Это соберёт:
- Go headless-бинарники (VK, Telemost, WBStream, DION)
- TypeScript бэкенд
- В один Docker-образ

### 4. Открыть панель

Панель доступна на `http://<IP-вашего-VDS>:<WEB_PORT>`

Например: `http://123.45.67.89:8080`

### 5. Загрузить cookies

1. Откройте вкладку **Cookies** в панели
2. Загрузите JSON-файлы куки для нужных платформ
3. Куки можно экспортировать из Electron `creator-app` (кнопка "Export Cookies")

### 6. Создать подключение

1. Нажмите **New Connection**
2. Укажите алиас, выберите платформу
3. Опционально вставьте ссылку/room для подключения к существующему звонку
4. Нажмите **Create** — подключение запустится автоматически

---

## Настройка порта

Порт настраивается через переменную `WEB_PORT` в `.env`:

```env
WEB_PORT=8080
```

Порт пробрасывается и внутрь контейнера, и наружу автоматически.

Для смены порта на работающем контейнере:

```bash
# Отредактировать .env
nano .env

# Перезапустить
docker compose down
docker compose up -d
```

---

## Все переменные окружения

| Переменная | По умолчанию | Описание |
|------------|-------------|----------|
| `WEB_PORT` | `3000` | Порт веб-интерфейса |
| `WEB_USER` | `admin` | Логин Basic Auth |
| `WEB_PASS` | `changeme` | Пароль Basic Auth |
| `UPSTREAM_SOCKS` | _(пусто)_ | SOCKS5 прокси для исходящего трафика (host:port) |
| `UPSTREAM_USER` | _(пусто)_ | Логин SOCKS5 |
| `UPSTREAM_PASS` | _(пусто)_ | Пароль SOCKS5 |
| `RESOURCES` | `default` | Режим ресурсов: `default`, `moderate`, `unlimited` |
| `DEBUG` | `false` | Включить debug-логирование |

---

## Управление

```bash
# Просмотр логов контейнера
docker compose logs -f

# Перезапуск
docker compose restart

# Остановка
docker compose down

# Обновление (pull + пересборка)
cd whitelist-bypass
git pull
cd creator-web
docker compose up -d --build
```

---

## Данные и бэкап

Все данные хранятся в Docker volume `creator-data`:
- `connections.json` — сохранённые подключения
- `settings.json` — настройки
- `cookies/` — файлы куки платформ

Бэкап:
```bash
docker run --rm -v creator-web_creator-data:/data -v $(pwd):/backup alpine tar czf /backup/creator-data.tar.gz -C /data .
```

Восстановление:
```bash
docker run --rm -v creator-web_creator-data:/data -v $(pwd):/backup alpine tar xzf /backup/creator-data.tar.gz -C /data
```

---

## Без Docker (ручной запуск)

```bash
# 1. Собрать headless бинарники
cd headless/vk && go build -o ../../creator-web/bins/headless-vk-creator .
cd ../telemost && go build -o ../../creator-web/bins/headless-telemost-creator .
cd ../wbstream && go build -o ../../creator-web/bins/headless-wbstream-creator .
cd ../dion && go build -o ../../creator-web/bins/headless-dion-creator .

# 2. Установить зависимости и собрать web-приложение
cd creator-web
npm install
npm run build

# 3. Настроить
cp .env.example .env
nano .env
# Указать BINS_DIR=./bins

# 4. Запустить
node dist/server.js
```

---

## REST API

| Метод | Путь | Описание |
|-------|------|----------|
| `GET` | `/api/connections` | Список подключений |
| `POST` | `/api/connections` | Создать подключение |
| `PUT` | `/api/connections/:id` | Обновить алиас/настройки |
| `DELETE` | `/api/connections/:id` | Удалить подключение |
| `POST` | `/api/connections/:id/start` | Запустить |
| `POST` | `/api/connections/:id/stop` | Остановить |
| `GET` | `/api/connections/:id/logs` | Получить логи |
| `GET` | `/api/connections/:id/qr` | QR-код join-ссылки |
| `GET` | `/api/cookies` | Статус cookie-файлов |
| `POST` | `/api/cookies/:platform` | Загрузить cookies (multipart) |
| `DELETE` | `/api/cookies/:platform` | Удалить cookies |
| `GET` | `/api/settings` | Текущие настройки |
| `PUT` | `/api/settings` | Обновить настройки |
