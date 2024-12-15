package main

import (
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sync"
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
	// Create data directory if it doesn't exist
	if err := os.MkdirAll("./data", 0700); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := "./data/database.db"

	// Create the database file if it doesn't exist
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		file, err := os.Create(dbPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create database file: %w", err)
		}
		file.Close()
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
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
			congrats_message TEXT,
			PRIMARY KEY (user_id, completed_at),
			FOREIGN KEY (user_id) REFERENCES participants(user_id)
		);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	return db, nil
}

func getRandomCongratsMessage() string {
	return CongratsMessages[rand.Intn(len(CongratsMessages))]
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

	msg := tgbotapi.NewMessage(message.Chat.ID, Messages["want_to_join"])
	msg.ReplyMarkup = keyboard
	_, err = b.api.Send(msg)
	return err
}

func (b *Bot) getParticipantsList() ([]struct {
	Name      string
	Completed bool
	Streak    int
}, error) {
	today := time.Now().Format("2006-01-02")
	rows, err := b.db.Query(`
		SELECT 
			COALESCE(p.display_name, p.username) as name,
			CASE WHEN dc.completed_at IS NOT NULL THEN 1 ELSE 0 END as completed,
			p.user_id
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
		Streak    int
	}
	for rows.Next() {
		var p struct {
			Name      string
			Completed bool
			Streak    int
		}
		var userID int64
		if err := rows.Scan(&p.Name, &p.Completed, &userID); err != nil {
			return nil, err
		}
		p.Streak, err = b.getIndividualStreak(userID)
		if err != nil {
			return nil, err
		}
		participants = append(participants, p)
	}
	return participants, nil
}

func (b *Bot) getIndividualStreak(userID int64) (int, error) {
	// Start from yesterday and go backwards to get the base streak
	currentDate := time.Now().AddDate(0, 0, -1)
	consecutiveDays := 0

	// Get base streak (not including today)
	for {
		dateStr := currentDate.Format("2006-01-02")

		var completed bool
		err := b.db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM daily_completions 
				WHERE user_id = ? AND completed_at = ?
			)
		`, userID, dateStr).Scan(&completed)

		if err != nil {
			return 0, err
		}

		if !completed {
			break
		}

		consecutiveDays++
		currentDate = currentDate.AddDate(0, 0, -1)
	}

	// Check if completed today
	today := time.Now().Format("2006-01-02")
	var completedToday bool
	err := b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM daily_completions 
			WHERE user_id = ? AND completed_at = ?
		)
	`, userID, today).Scan(&completedToday)

	if err != nil {
		return 0, err
	}

	// Add today to streak if completed
	if completedToday {
		consecutiveDays++
	}

	return consecutiveDays, nil
}

func (b *Bot) handleJoinChallenge(query *tgbotapi.CallbackQuery) error {
	msg := tgbotapi.NewMessage(query.Message.Chat.ID, Messages["enter_name"])
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

	// Get weekday in Russian
	weekdays := map[string]string{
		"Monday":    "Понедельник",
		"Tuesday":   "Вторник",
		"Wednesday": "Среда",
		"Thursday":  "Четверг",
		"Friday":    "Пятница",
		"Saturday":  "Суббота",
		"Sunday":    "Воскресенье",
	}
	currentWeekday := weekdays[time.Now().Weekday().String()]

	currentDate := time.Now().Format("02.01.2006")
	response := fmt.Sprintf("%s, %s\n", currentWeekday, currentDate)

	response += "Участники:\n\n"

	for _, p := range participants {
		status := "⏳"
		if p.Completed {
			status = "✅"
		}

		response += fmt.Sprintf("- %s %s (%d %s)\n\n", status, p.Name, p.Streak, getDayWord(p.Streak))
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

	// Add streak information to the response
	streak, err := b.getConsecutiveCompletionDays()
	if err != nil {
		return err
	}

	response += fmt.Sprintf("\n🔥 Дней подряд: %d\n",
		streak,
	)

	// Create a reply keyboard with options
	replyKeyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("Обновить"),
			tgbotapi.NewKeyboardButton("Сделать зарядочку"),
		),
	)
	replyKeyboard.ResizeKeyboard = true // Make keyboard smaller
	replyKeyboard.Selective = true

	msg := tgbotapi.NewMessage(chatID, response)
	msg.ReplyMarkup = replyKeyboard
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
		// For reply keyboard, send a message instead of callback
		msg := tgbotapi.NewMessage(query.Message.Chat.ID, Messages["already_completed"])
		_, err := b.api.Send(msg)
		return err
	}

	congratsMessage := getRandomCongratsMessage()

	// Mark as completed with congrats message
	_, err = b.db.Exec(`
		INSERT INTO daily_completions (user_id, completed_at, congrats_message)
		VALUES (?, ?, ?)
	`, query.From.ID, today, congratsMessage)
	if err != nil {
		return err
	}

	// Send congrats message
	msg := tgbotapi.NewMessage(query.Message.Chat.ID, congratsMessage)
	_, err = b.api.Send(msg)
	if err != nil {
		return err
	}

	// Show updated list
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
		callback := tgbotapi.NewCallback(query.ID, Messages["no_completion_today"])
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

	callback := tgbotapi.NewCallback(query.ID, Messages["completion_cancelled"])
	if _, err := b.api.Request(callback); err != nil {
		return err
	}

	return b.sendParticipantsList(query.Message.Chat.ID, query.From.ID)
}

func (b *Bot) sendDailyReminders() error {
	today := time.Now().Format("2006-01-02")

	// Get all participants who haven't completed today's challenge
	rows, err := b.db.Query(`
		SELECT p.user_id, p.chat_id 
		FROM participants p
		LEFT JOIN daily_completions dc 
			ON p.user_id = dc.user_id 
			AND dc.completed_at = ?
		WHERE dc.user_id IS NULL
	`, today)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var userID, chatID int64
		if err := rows.Scan(&userID, &chatID); err != nil {
			log.Printf("Error scanning user: %v", err)
			continue
		}

		participants, err := b.getParticipantsList()
		if err != nil {
			log.Printf("Error getting participants list: %v", err)
			continue
		}

		response := Messages["reminder"] + "\n\nУчастники:\n\n"
		for _, p := range participants {
			status := "⏳"
			if p.Completed {
				status = "✅"
			}
			response += fmt.Sprintf("- %s %s (%d %s)\n\n", status, p.Name, p.Streak, getDayWord(p.Streak))
		}

		msg := tgbotapi.NewMessage(chatID, response)
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("Error sending reminder to user %d: %v", userID, err)
		}
	}
	return nil
}

func (b *Bot) getConsecutiveCompletionDays() (int, error) {
	// Start from yesterday and go backwards to get the base streak
	currentDate := time.Now().AddDate(0, 0, -1)
	consecutiveDays := 0

	// Get base streak (not including today)
	for {
		dateStr := currentDate.Format("2006-01-02")

		var completedCount int
		err := b.db.QueryRow(`
			SELECT COUNT(DISTINCT user_id) 
			FROM daily_completions 
			WHERE completed_at = ? AND user_id IN (
				SELECT user_id FROM participants
				WHERE joined_at <= ?
			)
		`, dateStr, dateStr).Scan(&completedCount)

		if err != nil {
			return 0, err
		}

		var totalParticipants int
		err = b.db.QueryRow(`
			SELECT COUNT(*) 
			FROM participants 
			WHERE joined_at <= ?
		`, dateStr).Scan(&totalParticipants)

		if err != nil {
			return 0, err
		}

		if completedCount != totalParticipants || totalParticipants == 0 {
			break
		}

		consecutiveDays++
		currentDate = currentDate.AddDate(0, 0, -1)
	}

	// Check if everyone completed today's challenge
	today := time.Now().Format("2006-01-02")
	var todayCompletedCount int
	err := b.db.QueryRow(`
		SELECT COUNT(DISTINCT user_id) 
		FROM daily_completions 
		WHERE completed_at = ? AND user_id IN (
			SELECT user_id FROM participants
			WHERE joined_at <= ?
		)
	`, today, today).Scan(&todayCompletedCount)

	if err != nil {
		return 0, err
	}

	var totalParticipants int
	err = b.db.QueryRow(`
		SELECT COUNT(*) 
		FROM participants 
		WHERE joined_at <= ?
	`, today).Scan(&totalParticipants)

	if err != nil {
		return 0, err
	}

	// Add today to streak if everyone completed
	if todayCompletedCount == totalParticipants && totalParticipants > 0 {
		consecutiveDays++
	}

	return consecutiveDays, nil
}

// Helper function to get the correct form of "день/дня/дней"
func getDayWord(days int) string {
	if days == 0 {
		return "дней"
	}
	if days%10 == 1 && days%100 != 11 {
		return "день"
	}
	if days%10 >= 2 && days%10 <= 4 && (days%100 < 10 || days%100 >= 20) {
		return "дня"
	}
	return "дней"
}

// TestFillCompletions fills in completion records for the specified number of days
// If notEveryoneCompletes is true, it will randomly skip some completions
func (b *Bot) TestFillCompletions(days int, notEveryoneCompletes bool) error {
	// Get all participants
	rows, err := b.db.Query(`SELECT user_id FROM participants`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var participants []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return err
		}
		participants = append(participants, userID)
	}

	// Fill completions for each day
	for i := days - 1; i >= 0; i-- {
		date := time.Now().AddDate(0, 0, -i).Format("2006-01-02")

		for _, userID := range participants {
			// If notEveryoneCompletes is true, randomly skip some completions
			if notEveryoneCompletes && rand.Float32() < 0.3 { // 30% chance to skip
				continue
			}

			congratsMessage := getRandomCongratsMessage()
			_, err = b.db.Exec(`
				INSERT OR REPLACE INTO daily_completions (user_id, completed_at, congrats_message)
				VALUES (?, ?, ?)
			`, userID, date, congratsMessage)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (b *Bot) sendLastChanceReminders() error {
	today := time.Now().Format("2006-01-02")

	// Get all participants who haven't completed today's challenge
	rows, err := b.db.Query(`
		SELECT p.user_id, p.chat_id 
		FROM participants p
		LEFT JOIN daily_completions dc 
			ON p.user_id = dc.user_id 
			AND dc.completed_at = ?
		WHERE dc.user_id IS NULL
	`, today)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var userID, chatID int64
		if err := rows.Scan(&userID, &chatID); err != nil {
			log.Printf("Error scanning user: %v", err)
			continue
		}

		participants, err := b.getParticipantsList()
		if err != nil {
			log.Printf("Error getting participants list: %v", err)
			continue
		}

		response := "Последний шанс!\n\nУчастники:\n\n"
		for _, p := range participants {
			status := "⏳"
			if p.Completed {
				status = "✅"
			}
			response += fmt.Sprintf("- %s %s (%d %s)\n\n", status, p.Name, p.Streak, getDayWord(p.Streak))
		}

		msg := tgbotapi.NewMessage(chatID, response)
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("Error sending last chance reminder to user %d: %v", userID, err)
		}
	}
	return nil
}

func main() {
	// Load env variables
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	db, err := initDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	botAPI, err := tgbotapi.NewBotAPI(os.Getenv("BOT_TOKEN"))
	if err != nil {
		log.Panic(err)
	}

	// Set up bot's config
	botAPI.Debug = true
	log.Printf("Authorized on account %s", botAPI.Self.UserName)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	bot := NewBot(botAPI, db)
	updates := botAPI.GetUpdatesChan(u)

	rand.Seed(time.Now().UnixNano())

	// Add ticker for daily reminders
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			now := time.Now()
			nextNoon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
			nextEvening := time.Date(now.Year(), now.Month(), now.Day(), 21, 0, 0, 0, now.Location())

			if now.After(nextNoon) {
				nextNoon = nextNoon.Add(24 * time.Hour)
			}
			if now.After(nextEvening) {
				nextEvening = nextEvening.Add(24 * time.Hour)
			}

			noonTimer := time.NewTimer(nextNoon.Sub(now))
			eveningTimer := time.NewTimer(nextEvening.Sub(now))

			select {
			case <-noonTimer.C:
				if err := bot.sendDailyReminders(); err != nil {
					log.Printf("Error sending daily reminders: %v", err)
				}
			case <-eveningTimer.C:
				if err := bot.sendLastChanceReminders(); err != nil {
					log.Printf("Error sending last chance reminders: %v", err)
				}
			}
		}
	}()

	for update := range updates {
		var err error
		if update.Message != nil {
			switch update.Message.Text {
			case "/start":
				err = bot.handleStart(update.Message)
			case "Обновить":
				err = bot.sendParticipantsList(update.Message.Chat.ID, update.Message.From.ID)
			case "Сделать зарядочку":
				// Create a fake callback query to reuse existing logic
				fakeQuery := &tgbotapi.CallbackQuery{
					Message: update.Message,
					From:    update.Message.From,
					Data:    "complete_challenge",
				}
				err = bot.handleCompleteChallenge(fakeQuery)
			case "/test10":
				err = bot.TestFillCompletions(10, false)
			case "/test5random":
				err = bot.TestFillCompletions(5, true)
			default:
				// Handle name response if applicable
				if update.Message.ReplyToMessage != nil {
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
			}
		} else if update.CallbackQuery != nil {
			// Keep existing callback query handling for the initial join button
			switch update.CallbackQuery.Data {
			case "join_challenge":
				err = bot.handleJoinChallenge(update.CallbackQuery)
			}
		}

		if err != nil {
			log.Printf("Error handling update: %v", err)
		}
	}

	// Wait for goroutine to finish (though it never will in practice)
	wg.Wait()
}
