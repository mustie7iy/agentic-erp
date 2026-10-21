package i18n

// The workspace's messages. Every string a page shows is one of these,
// looked up in the catalog of the person's locale; TestCatalogsComplete
// holds every locale file to exactly this list.
const (
	AppName       Key = "app.name"
	PageTitle     Key = "page.title" // the page's own title, then the application's name
	SkipLink      Key = "layout.skip"
	NavLabel      Key = "nav.label"
	NavHome       Key = "nav.home"
	NavStatus     Key = "nav.status"
	FooterHelp    Key = "layout.help"
	LanguageName  Key = "language.name" // the locale's own name for itself
	HomeTitle     Key = "home.title"
	HomeNotice    Key = "home.notice.title"
	HomeNoticeMsg Key = "home.notice.body"
	HomeJobs      Key = "home.jobs.title"
	HomeJobsCount Key = "home.jobs.count" // plural on the number of jobs
	JobApprovals  Key = "job.approvals"
	JobInvoices   Key = "job.invoices"
	JobPayments   Key = "job.payments"
	JobBank       Key = "job.bank"
	JobJournal    Key = "job.journal"
	JobClose      Key = "job.close"
	JobReports    Key = "job.reports"
	TagPlanned    Key = "tag.planned"
	StatusTitle   Key = "status.title"
	StatusService Key = "status.service"
	StatusState   Key = "status.state"
	StatusRunning Key = "status.running"
	StatusVersion Key = "status.version"
	StatusRev     Key = "status.revision"
	StatusLang    Key = "status.language"
	StatusUnknown Key = "status.unknown"
	NotFoundTitle Key = "notfound.title"
	NotFoundBody  Key = "notfound.body"
	NotFoundHome  Key = "notfound.home"
)
