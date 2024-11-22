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
	db, err := sql.Open("sqlite3", "./data/database.db")
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
			congrats_message TEXT,
			PRIMARY KEY (user_id, completed_at),
			FOREIGN KEY (user_id) REFERENCES participants(user_id)
		);
	`)
	return db, err
}

var messages = map[string]string{
	"want_to_join":         "Ребята ежедневно кайфуют от зарядочки. Тоже хочешь?",
	"enter_name":           "Как к тебе обращаться?",
	"already_completed":    "Ты уже отметился, не суетись :)",
	"no_completion_today":  "У вас нет отметки о выполнении за сегодня",
	"completion_cancelled": "Отметка о выполнении отменена",
	"reminder":             "Не забудь сделать зарядочку сегодня! 💪",
}

var congratsMessages = []string{
	"Красава! 💪 Теперь можно и пельмешей навернуть",
	"Ого-го! Качаем мышцы, качаем жизнь! 🏋️‍♂️",
	"Вот это по-нашему! Теперь ты официально круче всех лежебок 😎",
	"Зарядка сделана, а значит день уже победный! 🏆",
	"Так держать, спортсмен! Олимпиада уже трепещет 🥇",
	"Ещё одна тренировка - и ты почти Дуэйн Джонсон! 💪😎",
	"Вау! Да ты просто машина! 🚀",
	"Спортивная братва уже гордится тобой! 🤜🤛",
	"Мышцы подкачаны, характер закален! 💪😤",
	"Теперь можно и пончик съесть, ты заслужил! 🍩",
	"Чак Норрис нервно курит в сторонке! 🥋",
	"Халк бы одобрил такую зарядку! 💚",
	"Теперь ты официально в клубе утренних чемпионов! 🌅",
	"Мастер спорта по утренней зарядке! 🎖",
	"Твои мышцы уже шепчут 'спасибо'! 🗣️",
	"Ещё немного, и придется расширять дверные проемы! 💪",
	"Спортивные боги аплодируют стоя! 👏",
	"Так-так-так, кто тут у нас такой молодец? 🤔",
	"Мотивация на максималках! 📈",
	"Качаем не только тело, но и силу воли! 🧠",
	"Теперь можно и селфи в спортзале! 🤳",
	"Твой организм говорит 'СПАСИБО'! ❤️",
	"Вот это настрой! Вот это характер! 🔥",
	"Ты просто космос! 🚀",
	"Зарядка level PRO! 🎮",
	"Спортивная элита пополнилась! 👑",
	"Вот это я понимаю - утренний герой! 🦸‍♂️",
	"Мышцы в шоке от такой заботы! 😱",
	"Теперь точно будет продуктивный день! 📆",
	"Зарядка сделана - можно и горы свернуть! ⛰️",
	"Вот это дисциплина! Вот это сила! 💪",
	"Утренний воин в деле! ⚔️",
	"Так-так-так, кто тут у нас такой спортивный? 🏃‍♂️",
	"Зарядка - check! Теперь мир твой! 🌍",
	"Вот это энергетика! Можно город освещать! ⚡",
	"Спортивный режим активирован! 🟢",
}

func getRandomCongratsMessage() string {
	return congratsMessages[rand.Intn(len(congratsMessages))]
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

	msg := tgbotapi.NewMessage(message.Chat.ID, messages["want_to_join"])
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
	msg := tgbotapi.NewMessage(query.Message.Chat.ID, messages["enter_name"])
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
	response := fmt.Sprintf("%s\n", currentDate)

	// Get today's congrats message if exists
	today := time.Now().Format("2006-01-02")
	var congratsMessage sql.NullString
	err = b.db.QueryRow(`
		SELECT congrats_message 
		FROM daily_completions 
		WHERE user_id = ? AND completed_at = ?
	`, userID, today).Scan(&congratsMessage)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if congratsMessage.Valid {
		response += fmt.Sprintf("%s\n\n", congratsMessage.String)
	}

	response += "Участники:\n\n"

	for _, p := range participants {
		status := "ещё нет"
		if p.Completed {
			status = "ДА"
		}

		response += fmt.Sprintf("- %s %s\n", p.Name, status)
	}

	// Check if user completed today
	today = time.Now().Format("2006-01-02")
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

	// Add streak information to the response
	streak, err := b.getConsecutiveCompletionDays()
	if err != nil {
		return err
	}

	if streak > 0 {
		response += fmt.Sprintf("\n🔥 Общий стрик: %d %s\n",
			streak,
			getDayWord(streak))
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
		callback := tgbotapi.NewCallback(query.ID, messages["already_completed"])
		_, err := b.api.Request(callback)
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

	callback := tgbotapi.NewCallback(query.ID, congratsMessage)
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
		callback := tgbotapi.NewCallback(query.ID, messages["no_completion_today"])
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

	callback := tgbotapi.NewCallback(query.ID, messages["completion_cancelled"])
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

		msg := tgbotapi.NewMessage(chatID, messages["reminder"])
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("Error sending reminder to user %d: %v", userID, err)
		}
	}
	return nil
}

func (b *Bot) getConsecutiveCompletionDays() (int, error) {
	// Get all participants
	rows, err := b.db.Query(`SELECT user_id FROM participants`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var participantIDs []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return 0, err
		}
		participantIDs = append(participantIDs, userID)
	}

	if len(participantIDs) == 0 {
		return 0, nil
	}

	// Start from today and go backwards
	currentDate := time.Now()
	consecutiveDays := 0

	for {
		dateStr := currentDate.Format("2006-01-02")

		// Check if all participants completed on this date
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

		// Get total participants who were members on that date
		var totalParticipants int
		err = b.db.QueryRow(`
			SELECT COUNT(*) 
			FROM participants 
			WHERE joined_at <= ?
		`, dateStr).Scan(&totalParticipants)

		if err != nil {
			return 0, err
		}

		// Break if not all participants completed or if we reach a date with no participants
		if completedCount != totalParticipants || totalParticipants == 0 {
			break
		}

		consecutiveDays++
		currentDate = currentDate.AddDate(0, 0, -1)
	}

	return consecutiveDays, nil
}

// Helper function to get the correct form of "день/дня/дней"
func getDayWord(days int) string {
	if days%10 == 1 && days%100 != 11 {
		return "день"
	}
	if days%10 >= 2 && days%10 <= 4 && (days%100 < 10 || days%100 >= 20) {
		return "дня"
	}
	return "дней"
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

	rand.Seed(time.Now().UnixNano())

	// Add ticker for daily reminders
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
			if now.After(next) {
				next = next.Add(24 * time.Hour)
			}
			timer := time.NewTimer(next.Sub(now))
			<-timer.C
			if err := bot.sendDailyReminders(); err != nil {
				log.Printf("Error sending daily reminders: %v", err)
			}
		}
	}()

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

	// Wait for goroutine to finish (though it never will in practice)
	wg.Wait()
}
