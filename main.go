package main

import (
	"database/sql"
	"log"
	"os"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

type Bot struct {
	api *tgbotapi.BotAPI
	db  *sql.DB
}

func NewBot(api *tgbotapi.BotAPI, db *sql.DB) *Bot {
	return &Bot{
		api: api,
		db:  db,
	}
}

func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "./database.db")
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS participants (
			user_id INTEGER PRIMARY KEY,
			username TEXT,
			chat_id INTEGER,
			joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	return db, err
}

func (b *Bot) handleStart(message *tgbotapi.Message) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Хочу 💪", "join_challenge"),
		),
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, "Ребята ежедневно кайфуют от зарядочки. Тоже хочешь?")
	msg.ReplyMarkup = keyboard
	_, err := b.api.Send(msg)
	return err
}

func (b *Bot) getParticipantsList() ([]string, error) {
	rows, err := b.db.Query(`
		SELECT username FROM participants
		ORDER BY joined_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var participants []string
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, err
		}
		participants = append(participants, "@"+username)
	}
	return participants, nil
}

func (b *Bot) handleJoinChallenge(query *tgbotapi.CallbackQuery) error {
	userID := query.From.ID
	username := query.From.UserName
	chatID := query.Message.Chat.ID

	_, err := b.db.Exec(`
		INSERT OR IGNORE INTO participants (user_id, username, chat_id)
		VALUES (?, ?, ?)
	`, userID, username, chatID)
	if err != nil {
		return err
	}

	participants, err := b.getParticipantsList()
	if err != nil {
		return err
	}

	response := "Добро пожаловать в зарядочку!\n\nУчастники:\n" + strings.Join(participants, "\n")

	callback := tgbotapi.NewCallback(query.ID, response)
	if _, err := b.api.Request(callback); err != nil {
		return err
	}

	msg := tgbotapi.NewMessage(chatID, response)
	_, err = b.api.Send(msg)
	return err
}

func main() {
	// Load env variables
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	botAPI, err := tgbotapi.NewBotAPI(os.Getenv("BOT_TOKEN"))
	if err != nil {
		log.Panic(err)
	}

	// Set up bot's config
	botAPI.Debug = true
	log.Printf("Authorized on account %s", botAPI.Self.UserName)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	db, err := initDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	bot := NewBot(botAPI, db)
	updates := botAPI.GetUpdatesChan(u)

	for update := range updates {
		var err error
		if update.Message != nil && update.Message.Text == "/start" {
			err = bot.handleStart(update.Message)
		} else if update.CallbackQuery != nil && update.CallbackQuery.Data == "join_challenge" {
			err = bot.handleJoinChallenge(update.CallbackQuery)
		}

		if err != nil {
			log.Printf("Error handling update: %v", err)
		}
	}
}
