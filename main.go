package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

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
			display_name TEXT,
			joined_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS pending_joins (
			user_id INTEGER PRIMARY KEY,
			chat_id INTEGER,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS daily_completions (
			user_id INTEGER,
			completed_at DATE,
			PRIMARY KEY (user_id, completed_at),
			FOREIGN KEY (user_id) REFERENCES participants(user_id)
		);
	`)
	return db, err
}

func (b *Bot) handleStart(message *tgbotapi.Message) error {
	// Check if user is already a participant
	var exists bool
	err := b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM participants 
			WHERE user_id = ?
		)
	`, message.From.ID).Scan(&exists)
	if err != nil {
		return err
	}

	if exists {
		return b.sendParticipantsList(message.Chat.ID, message.From.ID)
	}

	// ... existing keyboard and message code ...
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Хочу 💪", "join_challenge"),
		),
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, "Ребята ежедневно кайфуют от зарядочки. Тоже хочешь?")
	msg.ReplyMarkup = keyboard
	_, err = b.api.Send(msg)
	return err
}

func (b *Bot) getParticipantsList() ([]struct {
	Name      string
	Completed bool
}, error) {
	today := time.Now().Format("2006-01-02")
	rows, err := b.db.Query(`
		SELECT 
			COALESCE(p.display_name, p.username) as name,
			CASE WHEN dc.completed_at IS NOT NULL THEN 1 ELSE 0 END as completed
		FROM participants p
		LEFT JOIN daily_completions dc 
			ON p.user_id = dc.user_id 
			AND dc.completed_at = ?
		ORDER BY p.joined_at DESC
	`, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var participants []struct {
		Name      string
		Completed bool
	}
	for rows.Next() {
		var p struct {
			Name      string
			Completed bool
		}
		if err := rows.Scan(&p.Name, &p.Completed); err != nil {
			return nil, err
		}
		participants = append(participants, p)
	}
	return participants, nil
}

func (b *Bot) handleJoinChallenge(query *tgbotapi.CallbackQuery) error {
	// First, ask for the name
	callback := tgbotapi.NewCallback(query.ID, "Как вас записать в список?")
	if _, err := b.api.Request(callback); err != nil {
		return err
	}

	msg := tgbotapi.NewMessage(query.Message.Chat.ID, "Пожалуйста, введите ваше имя:")
	msg.ReplyMarkup = tgbotapi.ForceReply{ForceReply: true, Selective: true}
	_, err := b.api.Send(msg)

	// Store temporary state in DB to handle the name response
	_, err = b.db.Exec(`
		INSERT OR REPLACE INTO pending_joins (user_id, chat_id)
		VALUES (?, ?)
	`, query.From.ID, query.Message.Chat.ID)
	return err
}

func (b *Bot) handleNameResponse(message *tgbotapi.Message) error {
	userID := message.From.ID
	chatID := message.Chat.ID
	displayName := message.Text

	// Insert participant with custom name
	_, err := b.db.Exec(`
		INSERT OR REPLACE INTO participants (user_id, username, chat_id, display_name)
		VALUES (?, ?, ?, ?)
	`, userID, message.From.UserName, chatID, displayName)
	if err != nil {
		return err
	}

	// Remove from pending joins
	_, err = b.db.Exec(`DELETE FROM pending_joins WHERE user_id = ?`, userID)
	if err != nil {
		return err
	}

	return b.sendParticipantsList(chatID, userID)
}

func (b *Bot) sendParticipantsList(chatID int64, userID int64) error {
	participants, err := b.getParticipantsList()
	if err != nil {
		return err
	}

	currentDate := time.Now().Format("02.01.2006")
	response := fmt.Sprintf("%s\nУчастники:\n\n", currentDate)

	for _, p := range participants {
		status := "ещё нет"
		if p.Completed {
			status = "ДА"
		}
		response += fmt.Sprintf("- %s %s\n", p.Name, status)
	}

	// Check if user completed today
	today := time.Now().Format("2006-01-02")
	var completed bool
	err = b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM daily_completions 
			WHERE user_id = ? AND completed_at = ?
		)
	`, userID, today).Scan(&completed)
	if err != nil {
		return err
	}

	var actionButton tgbotapi.InlineKeyboardButton
	if completed {
		actionButton = tgbotapi.NewInlineKeyboardButtonData("Отменить зарядочку", "undo_complete")
	} else {
		actionButton = tgbotapi.NewInlineKeyboardButtonData("Сделать зарядочку", "complete_challenge")
	}

	msg := tgbotapi.NewMessage(chatID, response)
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Обновить", "update_list"),
			actionButton,
		),
	)
	_, err = b.api.Send(msg)
	return err
}

func (b *Bot) handleUpdateList(query *tgbotapi.CallbackQuery) error {
	// Add callback acknowledgment
	callback := tgbotapi.NewCallback(query.ID, "")
	if _, err := b.api.Request(callback); err != nil {
		return err
	}

	return b.sendParticipantsList(query.Message.Chat.ID, query.From.ID)
}

func (b *Bot) handleCompleteChallenge(query *tgbotapi.CallbackQuery) error {
	today := time.Now().Format("2006-01-02")

	// Check if already completed today
	var completed bool
	err := b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM daily_completions 
			WHERE user_id = ? AND completed_at = ?
		)
	`, query.From.ID, today).Scan(&completed)
	if err != nil {
		return err
	}

	if completed {
		callback := tgbotapi.NewCallback(query.ID, "Ты уже отметился, не суетись :)")
		_, err := b.api.Request(callback)
		return err
	}

	// Mark as completed
	_, err = b.db.Exec(`
		INSERT INTO daily_completions (user_id, completed_at)
		VALUES (?, ?)
	`, query.From.ID, today)
	if err != nil {
		return err
	}

	callback := tgbotapi.NewCallback(query.ID, "Отлично! Так держать! 💪")
	if _, err := b.api.Request(callback); err != nil {
		return err
	}

	return b.sendParticipantsList(query.Message.Chat.ID, query.From.ID)
}

func (b *Bot) handleUndoComplete(query *tgbotapi.CallbackQuery) error {
	today := time.Now().Format("2006-01-02")

	// Check if completed today
	var completed bool
	err := b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM daily_completions 
			WHERE user_id = ? AND completed_at = ?
		)
	`, query.From.ID, today).Scan(&completed)
	if err != nil {
		return err
	}

	if !completed {
		callback := tgbotapi.NewCallback(query.ID, "У вас нет отметки о выполнении за сегодня")
		_, err := b.api.Request(callback)
		return err
	}

	// Remove completion
	_, err = b.db.Exec(`
		DELETE FROM daily_completions 
		WHERE user_id = ? AND completed_at = ?
	`, query.From.ID, today)
	if err != nil {
		return err
	}

	callback := tgbotapi.NewCallback(query.ID, "Отметка о выполнении отменена")
	if _, err := b.api.Request(callback); err != nil {
		return err
	}

	return b.sendParticipantsList(query.Message.Chat.ID, query.From.ID)
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
		if update.Message != nil {
			if update.Message.Text == "/start" {
				err = bot.handleStart(update.Message)
			} else if update.Message.ReplyToMessage != nil {
				// Check if user is in pending_joins
				var exists bool
				err = bot.db.QueryRow(`
					SELECT EXISTS(
						SELECT 1 FROM pending_joins 
						WHERE user_id = ? AND chat_id = ?
					)
				`, update.Message.From.ID, update.Message.Chat.ID).Scan(&exists)

				if err == nil && exists {
					err = bot.handleNameResponse(update.Message)
				}
			}
		} else if update.CallbackQuery != nil {
			switch update.CallbackQuery.Data {
			case "join_challenge":
				err = bot.handleJoinChallenge(update.CallbackQuery)
			case "update_list":
				err = bot.handleUpdateList(update.CallbackQuery)
			case "complete_challenge":
				err = bot.handleCompleteChallenge(update.CallbackQuery)
			case "undo_complete":
				err = bot.handleUndoComplete(update.CallbackQuery)
			}
		}

		if err != nil {
			log.Printf("Error handling update: %v", err)
		}
	}
}
