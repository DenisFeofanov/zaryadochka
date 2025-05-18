package main

import (
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

type Bot struct {
	api    *tgbotapi.BotAPI
	db     *sql.DB
	logger *slog.Logger
}

func NewBot(api *tgbotapi.BotAPI, db *sql.DB) *Bot {
	return &Bot{
		api:    api,
		db:     db,
		logger: slog.Default(),
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
		CREATE TABLE IF NOT EXISTS achievements (
			user_id INTEGER,
			achievement_type TEXT,
			achieved_at DATE,
			PRIMARY KEY (user_id, achievement_type),
			FOREIGN KEY (user_id) REFERENCES participants(user_id)
		);
		CREATE TABLE IF NOT EXISTS bot_state (
			user_id INTEGER,
			chat_id INTEGER,
			state TEXT,
			context TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, chat_id)
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
			tgbotapi.NewInlineKeyboardButtonData(ButtonLabels["join_challenge"], "join_challenge"),
		),
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, Messages["want_to_join"])
	msg.ReplyMarkup = keyboard
	_, err = b.sendMessage(msg)
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
	_, err := b.sendMessage(msg)

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
	currentWeekday := WeekdayNames[time.Now().Weekday().String()]

	currentDate := time.Now().Format("02.01.2006")
	response := fmt.Sprintf("%s, %s\n", currentWeekday, currentDate)

	response += "\n"

	for _, p := range participants {
		status := StatusIcons["pending"]
		if p.Completed {
			status = StatusIcons["completed"]
		}

		response += fmt.Sprintf("- %s %s (%d %s)\n\n", status, p.Name, p.Streak, GetDayWord(p.Streak))
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

	// hidden for now
	// Add streak information to the response
	streak, err := b.getConsecutiveCompletionDays()
	if err != nil {
		return err
	}

	response += fmt.Sprintf("\n🔥 Совместных дней подряд: %d\n",
		streak,
	)

	// Add Walk of Fame
	fame, err := b.getWalkOfFame()
	if err != nil {
		return err
	}

	if len(fame) > 0 {
		response += Messages["hall_of_fame_separator"] + "\n"
		response += Messages["hall_of_fame"] + "\n\n"

		// Then list 100 achievers who haven't reached 365 yet
		has100 := false
		response += Messages["achievement_100"] + "\n"
		for _, f := range fame {
			if f.Achievement100 && !f.Achievement365 {
				has100 = true
				achievedDate := f.AchievedAt100.Format("02.01.2006")
				response += fmt.Sprintf("  • %s - %s (%s)\n", f.Name, Messages["achievement_reached"], achievedDate)
			}
		}

		if !has100 {
			response += Messages["no_achievements"] + "\n"
		}

		response += "\n"

		// First list 365 achievers
		hasLegends := false
		response += Messages["achievement_365"] + "\n"
		for _, f := range fame {
			if f.Achievement365 {
				hasLegends = true
				achievedDate := f.AchievedAt365.Format("02.01.2006")
				response += fmt.Sprintf("  • %s - %s (%s)\n", f.Name, Messages["achievement_reached"], achievedDate)
			}
		}

		if !hasLegends {
			response += Messages["no_achievements"]
		}
	}

	// Create a reply keyboard with options
	replyKeyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(ButtonLabels["update"]),
			tgbotapi.NewKeyboardButton(ButtonLabels["mark_yesterday"]),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton(ButtonLabels["do_exercise"]),
		),
	)
	replyKeyboard.ResizeKeyboard = true // Make keyboard smaller
	replyKeyboard.Selective = true

	msg := tgbotapi.NewMessage(chatID, response)
	msg.ReplyMarkup = replyKeyboard
	_, err = b.sendMessage(msg)
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

	// Get current streak to check for achievements
	streak, err := b.getIndividualStreak(query.From.ID)
	if err != nil {
		return err
	}

	// Check and record achievements if applicable
	if err := b.checkAndRecordAchievements(query.From.ID, streak); err != nil {
		return err
	}

	// Send congrats message
	msg := tgbotapi.NewMessage(query.Message.Chat.ID, congratsMessage)
	_, err = b.sendMessage(msg)
	if err != nil {
		return err
	}

	// Show updated list
	return b.sendParticipantsList(query.Message.Chat.ID, query.From.ID)
}

func (b *Bot) handleMarkYesterday(message *tgbotapi.Message) error {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	userID := message.From.ID
	chatID := message.Chat.ID

	// Check if already completed yesterday
	var completed bool
	err := b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM daily_completions 
			WHERE user_id = ? AND completed_at = ?
		)
	`, userID, yesterday).Scan(&completed)
	if err != nil {
		b.logger.Error("db error checking yesterday's completion", "error", err, "user_id", userID)
		errMsg := tgbotapi.NewMessage(chatID, Messages["error_try_later"])
		b.sendMessage(errMsg)
		return err
	}

	if completed {
		msg := tgbotapi.NewMessage(chatID, Messages["already_completed_yesterday"])
		_, errSend := b.sendMessage(msg)
		if errSend != nil {
			b.logger.Error("failed to send 'already_completed_yesterday' message", "error", errSend, "user_id", userID)
		}
		return nil
	}

	congratsMessage := getRandomCongratsMessage()

	// Mark yesterday as completed
	_, err = b.db.Exec(`
		INSERT INTO daily_completions (user_id, completed_at, congrats_message)
		VALUES (?, ?, ?)
	`, userID, yesterday, congratsMessage)
	if err != nil {
		b.logger.Error("db error inserting yesterday's completion", "error", err, "user_id", userID)
		errMsg := tgbotapi.NewMessage(chatID, Messages["error_marking_yesterday"])
		b.sendMessage(errMsg)
		return err
	}

	// Get current streak to check for achievements
	streak, err := b.getIndividualStreak(userID)
	if err != nil {
		b.logger.Error("failed to get individual streak after marking yesterday", "error", err, "user_id", userID)
	} else {
		if errAch := b.checkAndRecordAchievements(userID, streak); errAch != nil {
			b.logger.Error("failed to check/record achievements after marking yesterday", "error", errAch, "user_id", userID)
		}
	}

	successMsg := tgbotapi.NewMessage(chatID, Messages["yesterday_marked_success"])
	_, errSend := b.sendMessage(successMsg)
	if errSend != nil {
		b.logger.Error("failed to send 'yesterday_marked_success' message", "error", errSend, "user_id", userID)
	}

	return b.sendParticipantsList(chatID, userID)
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
			b.logger.Error("error scanning user", "error", err)
			continue
		}

		participants, err := b.getParticipantsList()
		if err != nil {
			b.logger.Error("error getting participants list", "error", err)
			continue
		}

		response := Messages["reminder"] + "\n\nУчастники:\n\n"
		for _, p := range participants {
			status := StatusIcons["pending"]
			if p.Completed {
				status = StatusIcons["completed"]
			}
			response += fmt.Sprintf("- %s %s (%d %s)\n\n", status, p.Name, p.Streak, GetDayWord(p.Streak))
		}

		msg := tgbotapi.NewMessage(chatID, response)
		if _, err := b.sendMessage(msg); err != nil {
			b.logger.Error("error sending reminder",
				"user_id", userID,
				"error", err,
			)
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

// SetUserStreak sets a specific streak for a user by filling in completion records
// for consecutive days leading up to today
func (b *Bot) SetUserStreak(userID int64, streakDays int) error {
	// First, check if the user exists
	var exists bool
	err := b.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM participants 
			WHERE user_id = ?
		)
	`, userID).Scan(&exists)
	if err != nil {
		return err
	}

	if !exists {
		return fmt.Errorf("user with ID %d does not exist", userID)
	}

	// Clear existing streak data first to avoid conflicts
	_, err = b.db.Exec(`
		DELETE FROM daily_completions 
		WHERE user_id = ? AND completed_at >= date('now', ?) AND completed_at <= date('now')
	`, userID, fmt.Sprintf("-%d days", streakDays))
	if err != nil {
		return err
	}

	// Fill completions for each day in the streak
	for i := streakDays - 1; i >= 0; i-- {
		date := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		congratsMessage := getRandomCongratsMessage()

		_, err = b.db.Exec(`
			INSERT INTO daily_completions (user_id, completed_at, congrats_message)
			VALUES (?, ?, ?)
		`, userID, date, congratsMessage)
		if err != nil {
			return err
		}
	}

	// Check for achievements after setting the streak
	streak, err := b.getIndividualStreak(userID)
	if err != nil {
		return err
	}

	return b.checkAndRecordAchievements(userID, streak)
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
			b.logger.Error("error scanning user", "error", err)
			continue
		}

		participants, err := b.getParticipantsList()
		if err != nil {
			b.logger.Error("error getting participants list", "error", err)
			continue
		}

		response := Messages["last_chance"] + "\n\nУчастники:\n\n"
		for _, p := range participants {
			status := StatusIcons["pending"]
			if p.Completed {
				status = StatusIcons["completed"]
			}
			response += fmt.Sprintf("- %s %s (%d %s)\n\n", status, p.Name, p.Streak, GetDayWord(p.Streak))
		}

		msg := tgbotapi.NewMessage(chatID, response)
		if _, err := b.sendMessage(msg); err != nil {
			b.logger.Error("error sending last chance reminder",
				"user_id", userID,
				"error", err,
			)
		}
	}
	return nil
}

// Helper functions for consistent logging
func getChatID(update tgbotapi.Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil {
		return update.CallbackQuery.Message.Chat.ID
	}
	return 0
}

func getUserID(update tgbotapi.Update) int64 {
	if update.Message != nil {
		return update.Message.From.ID
	}
	if update.CallbackQuery != nil {
		return update.CallbackQuery.From.ID
	}
	return 0
}

func getUpdateType(update tgbotapi.Update) string {
	if update.Message != nil {
		return "message"
	}
	if update.CallbackQuery != nil {
		return "callback_query"
	}
	return "unknown"
}

// Helper method for sending messages with logging
func (b *Bot) sendMessage(msg tgbotapi.MessageConfig) (tgbotapi.Message, error) {
	sent, err := b.api.Send(msg)
	if err != nil {
		b.logger.Error("failed to send message",
			"chat_id", msg.ChatID,
			"text", msg.Text,
			"error", err,
		)
		return sent, err
	}

	b.logger.Info("sent message",
		"chat_id", msg.ChatID,
		"text", msg.Text,
		"message_id", sent.MessageID,
	)
	return sent, nil
}

// checkAndRecordAchievements checks if a user has reached any milestone streaks
// and records the achievement if they have.
func (b *Bot) checkAndRecordAchievements(userID int64, streak int) error {
	// Check for milestone achievements
	if streak >= 100 {
		// Check if user already has the 100+ days achievement
		var exists bool
		err := b.db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM achievements 
				WHERE user_id = ? AND achievement_type = '100_days'
			)
		`, userID).Scan(&exists)
		if err != nil {
			return err
		}

		// Record the achievement if not yet achieved
		if !exists {
			_, err = b.db.Exec(`
				INSERT INTO achievements (user_id, achievement_type, achieved_at)
				VALUES (?, '100_days', date('now'))
			`, userID)
			if err != nil {
				return err
			}

			// Send congratulatory message
			var chatID int64
			err = b.db.QueryRow(`SELECT chat_id FROM participants WHERE user_id = ?`, userID).Scan(&chatID)
			if err != nil {
				return err
			}

			var name string
			err = b.db.QueryRow(`SELECT COALESCE(display_name, username) FROM participants WHERE user_id = ?`, userID).Scan(&name)
			if err != nil {
				return err
			}

			msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(Messages["achievement_100_congrats"]))
			_, err = b.sendMessage(msg)
			if err != nil {
				return err
			}
		}
	}

	if streak >= 365 {
		// Check if user already has the 365+ days achievement
		var exists bool
		err := b.db.QueryRow(`
			SELECT EXISTS(
				SELECT 1 FROM achievements 
				WHERE user_id = ? AND achievement_type = '365_days'
			)
		`, userID).Scan(&exists)
		if err != nil {
			return err
		}

		// Record the achievement if not yet achieved
		if !exists {
			_, err = b.db.Exec(`
				INSERT INTO achievements (user_id, achievement_type, achieved_at)
				VALUES (?, '365_days', date('now'))
			`, userID)
			if err != nil {
				return err
			}

			// Send congratulatory message
			var chatID int64
			err = b.db.QueryRow(`SELECT chat_id FROM participants WHERE user_id = ?`, userID).Scan(&chatID)
			if err != nil {
				return err
			}

			var name string
			err = b.db.QueryRow(`SELECT COALESCE(display_name, username) FROM participants WHERE user_id = ?`, userID).Scan(&name)
			if err != nil {
				return err
			}

			msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(Messages["achievement_365_congrats"]))
			_, err = b.sendMessage(msg)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// getWalkOfFame returns all participants who have achieved milestone streaks
func (b *Bot) getWalkOfFame() ([]struct {
	Name           string
	Achievement100 bool
	Achievement365 bool
	AchievedAt100  time.Time
	AchievedAt365  time.Time
}, error) {
	rows, err := b.db.Query(`
		SELECT 
			COALESCE(p.display_name, p.username) as name,
			a100.user_id IS NOT NULL as achievement_100,
			a365.user_id IS NOT NULL as achievement_365,
			a100.achieved_at as achieved_at_100,
			a365.achieved_at as achieved_at_365
		FROM participants p
		LEFT JOIN achievements a100 
			ON p.user_id = a100.user_id 
			AND a100.achievement_type = '100_days'
		LEFT JOIN achievements a365 
			ON p.user_id = a365.user_id 
			AND a365.achievement_type = '365_days'
		WHERE a100.user_id IS NOT NULL OR a365.user_id IS NOT NULL
		ORDER BY 
			a365.user_id IS NULL, 
			a365.achieved_at DESC,
			a100.achieved_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fame []struct {
		Name           string
		Achievement100 bool
		Achievement365 bool
		AchievedAt100  time.Time
		AchievedAt365  time.Time
	}

	for rows.Next() {
		var f struct {
			Name           string
			Achievement100 bool
			Achievement365 bool
			AchievedAt100  time.Time
			AchievedAt365  time.Time
		}
		var achieved100, achieved365 sql.NullTime
		if err := rows.Scan(&f.Name, &f.Achievement100, &f.Achievement365, &achieved100, &achieved365); err != nil {
			return nil, err
		}

		if achieved100.Valid {
			f.AchievedAt100 = achieved100.Time
		}
		if achieved365.Valid {
			f.AchievedAt365 = achieved365.Time
		}

		fame = append(fame, f)
	}

	return fame, nil
}

// handleListUserIDs lists all participants with their IDs
func (b *Bot) handleListUserIDs(message *tgbotapi.Message) error {
	rows, err := b.db.Query(`
		SELECT 
			user_id, 
			COALESCE(display_name, username) as name
		FROM participants
		ORDER BY joined_at
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	response := "📋 Список участников и их ID:\n\n"

	for rows.Next() {
		var userID int64
		var name string
		if err := rows.Scan(&userID, &name); err != nil {
			return err
		}

		response += fmt.Sprintf("👤 %s - ID: %d\n", name, userID)
	}

	response += "\nДля установки серии используйте команду:\n/setstreak ID количествоДней"

	msg := tgbotapi.NewMessage(message.Chat.ID, response)
	_, err = b.sendMessage(msg)
	return err
}

// handleAdjustStreak combines listing users and setting streak in one interactive command
func (b *Bot) handleAdjustStreak(message *tgbotapi.Message) error {
	// Step 1: Get the list of users
	rows, err := b.db.Query(`
		SELECT 
			user_id, 
			COALESCE(display_name, username) as name
		FROM participants
		ORDER BY joined_at
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Build a keyboard with buttons for each user
	var keyboard [][]tgbotapi.InlineKeyboardButton

	for rows.Next() {
		var userID int64
		var name string
		if err := rows.Scan(&userID, &name); err != nil {
			return err
		}

		// Create a button for each user with callback data in format "adjust_streak:userID:name"
		callbackData := fmt.Sprintf("adjust_streak:%d:%s", userID, name)
		// Truncate callback data if it's too long (Telegram has a 64 byte limit)
		if len(callbackData) > 64 {
			// Keep the userID part intact but truncate the name
			callbackData = fmt.Sprintf("adjust_streak:%d:%s", userID, name[:40])
		}

		row := []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("👤 %s", name),
				callbackData,
			),
		}
		keyboard = append(keyboard, row)
	}

	// Send the message with the keyboard
	msg := tgbotapi.NewMessage(message.Chat.ID, "Выберите пользователя для установки серии зарядок:")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(keyboard...)
	_, err = b.sendMessage(msg)
	return err
}

// handleAdjustStreakCallback processes the callback when a user is selected for streak adjustment
func (b *Bot) handleAdjustStreakCallback(query *tgbotapi.CallbackQuery) error {
	// Parse the callback data: "adjust_streak:userID:name"
	parts := strings.Split(query.Data, ":")
	if len(parts) < 3 {
		return fmt.Errorf("invalid callback data format")
	}

	userID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return err
	}

	// Get the name (parts[2] might be truncated, so fetch from DB to be sure)
	var name string
	err = b.db.QueryRow(`SELECT COALESCE(display_name, username) FROM participants WHERE user_id = ?`, userID).Scan(&name)
	if err != nil {
		// If there's an error, use what we have from the callback
		name = parts[2]
	}

	// Create a keyboard with buttons for common streak values
	keyboard := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("0 дней", fmt.Sprintf("set_streak:%d:0", userID)),
			tgbotapi.NewInlineKeyboardButtonData("7 дней", fmt.Sprintf("set_streak:%d:7", userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("30 дней", fmt.Sprintf("set_streak:%d:30", userID)),
			tgbotapi.NewInlineKeyboardButtonData("100 дней", fmt.Sprintf("set_streak:%d:100", userID)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("Другое значение ✏️", fmt.Sprintf("custom_streak:%d", userID)),
		},
	}

	// Clear the previous keyboard and show the streak options
	editMsg := tgbotapi.NewEditMessageText(
		query.Message.Chat.ID,
		query.Message.MessageID,
		fmt.Sprintf("Установка серии для пользователя 👤 %s\nВыберите количество дней:", name),
	)
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}

	_, err = b.api.Send(editMsg)
	return err
}

// handleSetStreakCallback processes the callback when a streak value is selected
func (b *Bot) handleSetStreakCallback(query *tgbotapi.CallbackQuery) error {
	// Parse the callback data: "set_streak:userID:days"
	parts := strings.Split(query.Data, ":")
	if len(parts) != 3 {
		return fmt.Errorf("invalid callback data format")
	}

	userID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return err
	}

	days, err := strconv.Atoi(parts[2])
	if err != nil {
		return err
	}

	// Set the streak
	err = b.SetUserStreak(userID, days)
	if err != nil {
		// Edit message to show error
		editMsg := tgbotapi.NewEditMessageText(
			query.Message.Chat.ID,
			query.Message.MessageID,
			fmt.Sprintf("❌ Ошибка при установке серии: %s", err.Error()),
		)
		_, err = b.api.Send(editMsg)
		return err
	}

	// Get the user's name
	var name string
	err = b.db.QueryRow(`SELECT COALESCE(display_name, username) FROM participants WHERE user_id = ?`, userID).Scan(&name)
	if err != nil {
		// If there's an error, use a generic name
		name = fmt.Sprintf("пользователя (ID: %d)", userID)
	}

	// Edit the message to show success
	editMsg := tgbotapi.NewEditMessageText(
		query.Message.Chat.ID,
		query.Message.MessageID,
		fmt.Sprintf("✅ Серия для %s установлена на %d %s", name, days, GetDayWord(days)),
	)
	_, err = b.api.Send(editMsg)
	if err != nil {
		return err
	}

	// Show updated list
	return b.sendParticipantsList(query.Message.Chat.ID, query.From.ID)
}

// handleCustomStreakCallback initiates the custom streak input process
func (b *Bot) handleCustomStreakCallback(query *tgbotapi.CallbackQuery) error {
	// Parse the callback data: "custom_streak:userID"
	parts := strings.Split(query.Data, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid callback data format")
	}

	userID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return err
	}

	// Get the user's name
	var name string
	err = b.db.QueryRow(`SELECT COALESCE(display_name, username) FROM participants WHERE user_id = ?`, userID).Scan(&name)
	if err != nil {
		// If there's an error, use a generic name
		name = fmt.Sprintf("пользователя (ID: %d)", userID)
	}

	// Save the state in the database to remember we're waiting for a custom streak value
	_, err = b.db.Exec(`
		INSERT OR REPLACE INTO bot_state (user_id, chat_id, state, context)
		VALUES (?, ?, 'waiting_custom_streak', ?)
	`, query.From.ID, query.Message.Chat.ID, strconv.FormatInt(userID, 10))
	if err != nil {
		return err
	}

	// Edit the message to prompt for custom streak input
	editMsg := tgbotapi.NewEditMessageText(
		query.Message.Chat.ID,
		query.Message.MessageID,
		fmt.Sprintf("Введите целое число для установки серии зарядок для пользователя 👤 %s:", name),
	)
	// Remove the inline keyboard
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{}

	_, err = b.api.Send(editMsg)
	return err
}

// handleCustomStreakInput processes the custom streak value entered by the user
func (b *Bot) handleCustomStreakInput(message *tgbotapi.Message) error {
	// Get the state from the database
	var state string
	var context string
	err := b.db.QueryRow(`
		SELECT state, context FROM bot_state 
		WHERE user_id = ? AND chat_id = ? AND state = 'waiting_custom_streak'
	`, message.From.ID, message.Chat.ID).Scan(&state, &context)

	if err != nil {
		// If no state is found, ignore
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	// Parse the target user ID from the context
	targetUserID, err := strconv.ParseInt(context, 10, 64)
	if err != nil {
		return err
	}

	// Parse the days value from the message
	days, err := strconv.Atoi(strings.TrimSpace(message.Text))
	if err != nil || days < 0 {
		msg := tgbotapi.NewMessage(message.Chat.ID, "❌ Пожалуйста, введите положительное целое число для серии зарядок.")
		_, err = b.sendMessage(msg)
		return err
	}

	// Set the streak
	err = b.SetUserStreak(targetUserID, days)
	if err != nil {
		msg := tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("❌ Ошибка при установке серии: %s", err.Error()))
		_, err = b.sendMessage(msg)
		return err
	}

	// Clear the state
	_, err = b.db.Exec(`DELETE FROM bot_state WHERE user_id = ? AND chat_id = ?`, message.From.ID, message.Chat.ID)
	if err != nil {
		return err
	}

	// Get the user's name
	var name string
	err = b.db.QueryRow(`SELECT COALESCE(display_name, username) FROM participants WHERE user_id = ?`, targetUserID).Scan(&name)
	if err != nil {
		// If there's an error, use a generic name
		name = fmt.Sprintf("пользователя (ID: %d)", targetUserID)
	}

	// Send success message
	msg := tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("✅ Серия для %s установлена на %d %s", name, days, GetDayWord(days)))
	_, err = b.sendMessage(msg)
	if err != nil {
		return err
	}

	// Show updated list
	return b.sendParticipantsList(message.Chat.ID, message.From.ID)
}

func main() {
	// Configure structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	// Load env variables
	err := godotenv.Load()
	if err != nil {
		slog.Error("failed to load .env file", "error", err)
		os.Exit(1)
	}

	db, err := initDB()
	if err != nil {
		slog.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	botAPI, err := tgbotapi.NewBotAPI(os.Getenv("BOT_TOKEN"))
	if err != nil {
		slog.Error("failed to create bot API", "error", err)
		os.Exit(1)
	}

	// Set up bot's config
	botAPI.Debug = false
	slog.Info("bot authorized successfully",
		"username", botAPI.Self.UserName,
		"debug_mode", botAPI.Debug,
	)
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
			loc, err := time.LoadLocation("Asia/Yekaterinburg")
			if err != nil {
				log.Fatalf("Error loading location: %v", err)
			}
			now := time.Now().In(loc)
			nextNoon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, loc)
			nextEvening := time.Date(now.Year(), now.Month(), now.Day(), 21, 0, 0, 0, loc)

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
					slog.Error("failed to send daily reminders",
						"error", err,
						"time", time.Now(),
					)
				}
			case <-eveningTimer.C:
				if err := bot.sendLastChanceReminders(); err != nil {
					slog.Error("failed to send last chance reminders",
						"error", err,
						"time", time.Now(),
					)
				}
			}
		}
	}()

	for update := range updates {
		var err error

		// Add context logging for each update
		logger := slog.With(
			"update_id", update.UpdateID,
			"chat_id", getChatID(update),
			"user_id", getUserID(update),
		)

		if update.Message != nil {
			logger.Info("received message",
				"text", update.Message.Text,
				"from", update.Message.From.UserName,
				"message_id", update.Message.MessageID,
			)
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
			case "Отметить за вчера":
				err = bot.handleMarkYesterday(update.Message)
			case "/listuserids":
				err = bot.handleListUserIDs(update.Message)
			case "/adjuststreak":
				err = bot.handleAdjustStreak(update.Message)
			default:
				// Check for commands with parameters
				if strings.HasPrefix(update.Message.Text, "/setstreak") {
					// Replace with the new command to avoid breaking existing functionality
					msg := tgbotapi.NewMessage(update.Message.Chat.ID, "Команда /setstreak устарела. Пожалуйста, используйте команду /adjuststreak для установки серии зарядок.")
					_, err = bot.sendMessage(msg)
				} else {
					// Check if we're waiting for a custom streak input
					var exists bool
					err = bot.db.QueryRow(`
						SELECT EXISTS(
							SELECT 1 FROM bot_state 
							WHERE user_id = ? AND chat_id = ? AND state = 'waiting_custom_streak'
						)
					`, update.Message.From.ID, update.Message.Chat.ID).Scan(&exists)

					if err == nil && exists {
						err = bot.handleCustomStreakInput(update.Message)
					} else if update.Message.ReplyToMessage != nil {
						// Handle name response if applicable
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
			}
		} else if update.CallbackQuery != nil {
			logger.Info("received callback query",
				"data", update.CallbackQuery.Data,
				"from", update.CallbackQuery.From.UserName,
			)

			// Extract the prefix from the callback data
			callbackData := update.CallbackQuery.Data
			var callbackPrefix string
			if strings.Contains(callbackData, ":") {
				callbackPrefix = strings.Split(callbackData, ":")[0]
			} else {
				callbackPrefix = callbackData
			}

			// Handle different callback types
			switch {
			case callbackData == "join_challenge":
				err = bot.handleJoinChallenge(update.CallbackQuery)
			case callbackData == "complete_challenge":
				err = bot.handleCompleteChallenge(update.CallbackQuery)
			case callbackData == "undo_complete":
				err = bot.handleUndoComplete(update.CallbackQuery)
			case callbackData == "update_list":
				err = bot.handleUpdateList(update.CallbackQuery)
			case callbackPrefix == "adjust_streak":
				err = bot.handleAdjustStreakCallback(update.CallbackQuery)
			case callbackPrefix == "set_streak":
				err = bot.handleSetStreakCallback(update.CallbackQuery)
			case callbackPrefix == "custom_streak":
				err = bot.handleCustomStreakCallback(update.CallbackQuery)
			}
		}

		if err != nil {
			logger.Error("failed to handle update",
				"error", err,
				"update_type", getUpdateType(update),
			)
		}
	}

	// Wait for goroutine to finish (though it never will in practice)
	wg.Wait()
}
