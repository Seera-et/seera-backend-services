Product Requirements Document (PRD)
Ethiopian Legal Intelligence Platform — MVP
Version:
1.0
Status:
MVP Specifi cation
Product Type:
AI-powered legal information and guidance platform
Primary Market:
Ethiopia
Architecture:
Unifi ed RAG + structured decision workfl ows

1. Executive Summary & Goals
   1.1 Product Vision
   The Ethiopian Legal Intelligence Platform is a web-based AI assistant thatmakes Ethiopian laws easier to
   search, understand, and apply to real-world situations
   .
   Instead of requiring users to manually search through hundreds of legaldocuments, the platform allows them to ask questions, explore legal sources,analyze their own documents, and receive guided recommendations groundedin Ethiopian legal sources.
   The core principle is:
   1.2 Problem
   Ethiopian legal information is often:
   Distributed across diff erent documents and institutions.
   Diffi cult to search semantically.
   Written in complex legal language.
   Diffi cult for entrepreneurs and ordinary citizens to interpret.
   Diffi cult to connect across multiple laws and regulations.
   Not easily accessible through modern search interfaces.
   Generic LLMs may also produce incorrect or outdated legal information.
   The AI does not invent the law. It retrieves relevant legal sourcesfi rst, reasons over them, and cites the source used for its answer.
   The platform addresses this through a centralized, searchable legal knowledgebase combined with RAG.
   1.3 Target Users
   User
   Primary Need
   Entrepreneurs
   Understand registration, licensing and businessrequirements
   Legal professionals
   Quickly search and cross-reference Ethiopian law
   Students/Researchers
   Explore and understand legal sources
   Businesses
   Understand regulatory and contractualobligations
   Developers
   Access a structured legal intelligence system/APIin future
   1.4 Core Value Proposition
   "Ask Ethiopian law. Understand the answer. See exactly whereit came from."
   The platform provides:
1. Natural-language legal search.
1. Structured legal document exploration.
1. Business/license recommendations.
1. Document-specifi c legal intelligence.
1. Article-level source citations.
1. MVP Goals & Success Metrics
   2.1 Product Goals
   The MVP should:
   Provide reliable answers grounded in Ethiopian legal documents.
   Make legal documents searchable and browsable.
   Guide entrepreneurs through common business setup scenarios.
   Allow users to analyze uploaded legal documents.
   Provide transparent source citations.
   Demonstrate production-quality AI/RAG engineering.
   2.2 Key Success Metrics
   Metric MVP Target
   Retrieval Precision@5 ≥ 80%
   Citation correctness ≥ 90%
   Grounded answer rate ≥ 90%
   Unsupported factual claims < 5%
   Average first-token latency < 3 sec
   Typical complete response < 10 sec
   Legal document search < 1 sec
   PDF ingestion < 60 sec for typical document
   Citation → source resolution 100%
   Accuracy should initially be measured using a manually created evaluation
   dataset of legal questions and expected source articles.
1. System Architecture
   3.1 High-Level Architecture
   ┌──────────────────┐
   │ Web Frontend │
   │ Next.js + React │
   └────────┬─────────┘
   │
   REST / SSE
   │
   ▼
   ┌──────────────────┐
1. Unified RAG Architecture
   All four AI-facing features share the same retrieval infrastructure.
   4.1 RAG Query Pipeline
   │ Go Backend │
   │ API Layer │
   └────────┬─────────┘
   │
   ┌──────────────┼──────────────┐
   │ │ │
   ▼ ▼ ▼
   Legal Q&A Business Advisor Document AI
   │ │ │
   └──────────────┼──────────────┘
   ▼
   ┌──────────────────┐
   │ RAG Engine │
   └────────┬─────────┘
   │
   ┌───────────┴──────────┐
   ▼ ▼
   Vector Search Keyword Search
   pgvector PostgreSQL FTS
   │ │
   └───────────┬──────────┘
   ▼
   Re-ranker
   │
   ▼
   LLM
   │
   ▼
   Answer + Citations
   User Question
   ↓
   Query Classification
   ↓
   Query Normalization
   ↓
   Hybrid Retrieval
   ┌────┴─────────────┐
   │ │
   ▼ ▼
   Vector Search Keyword Search
   │ │
   └────────┬─────────┘
1. Technical Stack
   Frontend
   Technology Purpose
   Next.js Web application
   React UI
   TypeScript Type safety
   Tailwind CSS Styling
   shadcn/ui UI components
   Server-Sent Events Streaming AI responses
   Backend
   Recommended: Go
   Technology Purpose
   Go API/backend
   Chi or Gin HTTP framework
   REST Primary API
   ↓
   Candidate Merge
   ↓
   Re-ranking
   ↓
   Top Relevant Chunks
   ↓
   Context Construction
   ↓
   LLM
   ↓
   Grounding Validation
   ↓
   Answer + Citations
   Technology Purpose
   SSE AI response streaming
   WebSocket Optional future real-time functionality
   OpenAPI API documentation
   For this MVP, REST + SSE is preferable to WebSockets because AI
   response streaming doesn't require a persistent bidirectional connection.
   Database
   PostgreSQL stores:
   Legal documents
   Articles
   Sections
   Chunks
   Metadata
   Embeddings
   Categories
   Citations
   Document versions
   Object storage stores:
   Original PDFs
   Word documents
   Uploaded user documents
   Processed document artifacts
1. AI Stack
   LLM Strategy
   The system should use an abstraction layer:
   PostgreSQL

- pgvector
  This prevents the application from becoming locked to a single provider.
  For MVP, use a high-quality hosted LLM for better reasoning and lower
  infrastructure complexity.
  Later, open-source models such as Qwen/Llama can be evaluated.
  Embeddings
  The embedding model must support English and preferably Amharic.
  Candidates:
  BGE-M3
  multilingual-e5
  Jina multilingual embeddings
  The embedding service should also be abstracted so models can be replaced
  without rebuilding the database architecture.

7. Feature Specifications
   Feature 1 — AI Legal Q&A
   Objective
   Allow users to ask natural-language questions about Ethiopian law.
   User Story
   Example
   Input:
   LLM Provider Interface
   │
   ┌─────┼──────┐
   ▼ ▼ ▼
   OpenAI Claude Local Model
   As a user, I want to ask a legal question in natural language so that I can
   understand the relevant Ethiopian laws without manually searching through
   documents.
   Output:
   UI
   Chat interface
   Suggested questions
   Streaming answer
   Citation cards
   Source preview
   Related articles
   Technical Flow
   Feature 2 — Ethiopian Legal Knowledge
   Explorer
   What are the requirements for establishing a private limited company in
   Ethiopia?
   A private limited company requires...
   [Explanation]
   Relevant sources:
   Commercial Code
   Article XX
   Business Registration Regulation
   Article XX
   Question
   ↓
   Query processor
   ↓
   Hybrid retrieval
   ↓
   Re-ranking
   ↓
   LLM
   ↓
   Grounding validator
   ↓
   Streaming response
   Objective
   Provide a structured searchable legal library.
   User Story
   Categories
   Example:
   UI
   Technical Requirements
   PostgreSQL full-text search
   Metadata filtering
   Article-level indexing
   As a researcher, I want to search and browse Ethiopian legal documents and
   articles so that I can directly inspect the original legal text.
   Commercial Law
   Tax
   Labor
   Investment
   Banking
   Intellectual Property
   Civil Law
   Criminal Law
   Business Licensing
   Search laws...
   Filters:
   [Domain]
   [Document Type]
   [Year]
   [Status]
   Results
   ────────────────
   Commercial Code
   Article 45
   ...
   Article 46
   ...
   Semantic search
   Pagination
   Version handling
   This feature can use the same vector database as Q&A.
   Feature 3 — Business & License Advisor
   Objective
   Guide users through starting a business in Ethiopia.
   User Story
   Workflow
   Output
   As an entrepreneur, I want to describe my planned business and receive
   guidance about the likely business structure, registration and licensing
   requirements.
   Start
   ↓
   Business type
   ↓
   Business activities
   ↓
   Number of founders
   ↓
   Foreign ownership?
   ↓
   Location
   ↓
   Other relevant questions
   ↓
   Decision Engine
   ↓
   RAG
   ↓
   Recommendation
   Recommended Structure
   Private Limited Company
   Architecture
   This feature is different from simple Q&A.
   The decision engine handles deterministic rules while the LLM handles naturallanguage
   interpretation and explanation.
   Feature 4 — Document Intelligence
   Objective
   Allow users to upload a PDF or Word document and interact with it using AI.
   User Stories
   Why:
   ...
   Registration steps:
1. ...
1. ...
1. ...
   Potential licenses:
   ✓ ...
   ✓ ...
   Legal basis:
   Commercial Code — Article XX
   Regulation — Article XX
   User Data
   ↓
   Decision Engine
   ↓
   Structured Recommendation
   ↓
   RAG Retrieval
   ↓
   Legal Validation
   ↓
   LLM Explanation
   ↓
   Recommendation + Sources
   Supported Inputs
   PDF
   DOCX
   Features
   Capability MVP
   Document summary ✅
   Document Q&A ✅
   Clause extraction ✅
   Legal comparison ✅
   Potential risk identification ✅
   Contract generation ❌
   Workflow
   As a user, I want to summarize a legal document.
   As a user, I want to ask questions about my document.
   As a user, I want to identify potentially problematic clauses based on
   Ethiopian law.
   Upload
   ↓
   Virus/file validation
   ↓
   Text extraction
   ↓
   OCR if required
   ↓
   Document chunking
   ↓
   Temporary embeddings
   ↓
   Document-specific retrieval
   ↓
   Retrieve Ethiopian legal sources
   ↓
   LLM
   The important architecture decision is that Document Intelligence can retrieve
   from two sources:
   This allows questions such as:
   Feature 5 — Citation & Source Grounding
   System
   This is a cross-platform capability rather than an independent user workflow.
   Objective
   Ensure AI answers are traceable to actual legal sources.
   Citation Model
   Every citation should point to an internal database entity:
   UI
   ↓
   Answer / Analysis
   User Document

- Ethiopian Legal Knowledge Base
  ↓
  RAG
  "Does this employment contract contain provisions that conflict with
  Ethiopian labor law?"
  Citation
  ↓
  Document
  ↓
  Version
  ↓
  Article
  ↓
  Exact text
  AI Answer:
  A company must...
  Important Design Decision
  Do not depend on an external URL to display citations.
  Store the original document and extracted legal content internally.
  The database should retain the official source URL when available for
  verification.

8. Data Model
   Core entities:
   Additional entities:
   [1] Commercial Code, Article 45
   ─────────────────────────────
   Source
   Commercial Code
   Article 45
   "...exact legal text..."
   Document version: 2022
   LegalDocument
   │
   ├── DocumentVersion
   │ │
   │ └── Article
   │ │
   │ └── Chunk
   │ │
   │ └── Embedding
   │
   └── Metadata
   LegalCategory
   Citation
   BusinessRule
   UserDocument
   DocumentChunk
9. Document Ingestion Pipeline
   Step 1 — Upload
   Admin uploads:
   Step 2 — Extraction
   If scanned:
   Step 3 — Structure Detection
   Detect:
   Prefer legal structure over arbitrary character-based chunks.
   Step 4 — Metadata
   Each chunk receives:
   Commercial_Code.pdf
   PDF
   ↓
   PyMuPDF
   ↓
   Extract text
   PDF
   ↓
   OCR
   ↓
   Text
   Chapter
   Section
   Article
   Paragraph
   Clause
   JSON
   Step 5 — Embedding
   Step 6 — Publish
   Only published documents are used for production RAG.
10. Content Management
    The Admin/Content Manager can:
    Upload
    {
    "document": "Commercial Code",
    "article": "45",
    "chapter": "Companies",
    "domain": "Commercial Law",
    "language": "English",
    "year": 2021,
    "version": "1"
    }
    Article text
    ↓
    Embedding model
    ↓
    Vector
    ↓
    pgvector
    Draft
    ↓
    Admin review
    ↓
    Published
    Upload document
    ↓
    Enter metadata
    ↓
    Process
    ↓
    Update
    Never overwrite historical legal versions.
11. Search Architecture
    The MVP should use hybrid search.
    Vector Search
    Finds semantic meaning.
    can retrieve:
    Keyword Search
    Finds exact legal terminology.
    should directly find Article 45.
    Combined
    Review
    ↓
    Publish
    Existing Law
    ↓
    New version
    ↓
    Create new DocumentVersion
    ↓
    Keep old version
    ↓
    Mark status
    "How can I establish my company?"
    "Formation of private limited companies..."
    "Article 45"
    Vector Search

- PostgreSQL FTS
  ↓
  Candidate Pool
  This is significantly better for legal documents than relying solely on
  embeddings.

12. Accuracy & Guardrails
    The system must follow these rules.
    Rule 1 — No evidence, no confident answer
    If relevant sources cannot be found:
    Rule 2 — Cite legal claims
    Important factual/legal claims should have citations.
    Rule 3 — Distinguish law from interpretation
    The UI should clearly separate:
    from:
    Rule 4 — Version awareness
    The system must consider whether a legal provision is:
    Current
    Superseded
    Repealed
    Unknown status
    ↓
    Re-ranker
    ↓
    Top K
    "I couldn't find sufficient information in the available Ethiopian legal sources
    to answer this reliably."
    Legal Source
    AI Interpretation
    Rule 5 — Disclaimer
    The platform should clearly state:
13. Performance Requirements
    Component
    Target
    Legal library search
    < 1 sec
    Retrieval
    < 1 sec
    First AI token
    < 3 sec
    Normal AI response
    < 10 sec
    PDF extraction
    < 60 sec
    Citation lookup
    < 300 ms
    API availability
    ≥ 99% during testing
    AI streaming should begin as soon as meaningful response generation startsrather than waiting for the entire answer.
14. Security & Data Handling
    Uploaded Documents
    User documents should:
    Be stored in private object storage.
    Have unique identifi ers.
    Not be publicly accessible.
    Have fi le-size/type restrictions.
    Be deleted according to an MVP retention policy.
    This platform provides legal information and AI-generated guidance basedon available sources. It does not constitute professional legal advice. Usersshould consult a qualifi ed Ethiopian legal professional for decisions requiringformal legal advice.
    API Security
    Even though authentication is out of scope:
    Validate all inputs.
    Rate-limit public endpoints.
    Sanitize uploaded fi les.
    Restrict fi le types.
    Prevent path traversal.
    Protect API keys server-side.
15. Out of Scope — MVP
    The following should
    not
    be implemented initially:
    Feature
    Status
    Authentication
    ❌
    User accounts
    ❌
    Payments
    ❌
    Mobile application
    ❌
    Custom LLM fi ne-tuning
    ❌
    Training an LLM from scratch
    ❌
    Contract generation
    ❌
    Lawyer marketplace
    ❌
    Legal case prediction
    ❌
    Court outcome prediction
    ❌
    Voice assistant
    ❌
    Automated government submission
    ❌
    Legal representation
    ❌
16. MVP End-to-End Architecture
    ┌─────────────────┐
    │ USER │
    └────────┬────────┘
    │
    ┌─────────▼─────────┐
    │ Next.js Web │
    └─────────┬─────────┘
    │
    REST + SSE
    │
    ┌─────────▼─────────┐
    │ Go Backend │
    └─────────┬─────────┘
    │
    ┌───────────────┼────────────────┐
    │ │ │
    ▼ ▼ ▼
    Legal Q&A Business Advisor Document AI
    │ │ │
    └───────────────┼────────────────┘
    ▼
    ┌────────────────┐
    │ RAG Engine │
    └───────┬────────┘
    │
    ┌──────────┴──────────┐
    ▼ ▼
    Vector Search Keyword Search
    pgvector PostgreSQL FTS
    │ │
    └──────────┬──────────┘
    ▼
    Re-ranker
    │
    ▼
    LLM
    │
    Grounding Check
    │
    ┌────────▼────────┐
    │ Answer + Source │
    └─────────────────┘
    ADMIN
    │
    ▼
    Document Upload
    │
17. MVP Definition of Done
    The MVP is complete when a user can:
18. Ask an Ethiopian legal question and receive a grounded streaming
    answer.
19. Search and browse Ethiopian laws down to the article level.
20. Describe a planned business and receive a structured recommendation
    with legal sources.
21. Upload a PDF/DOCX, summarize it, ask questions, and identify potential
    legal issues.
22. Click every citation and see the exact source text and document
    metadata inside the application.
23. Add/update legal documents through an internal content-management
    workflow.
24. Demonstrate measurable retrieval and citation accuracy using an
    evaluation dataset.
    The resulting system is not just an "AI chatbot"; it is an end-to-end legal
    information retrieval and reasoning platform combining document
    engineering, hybrid search, vector databases, backend architecture, LLM
    integration, structured decision systems, and source-grounded generation.
    ▼
    PDF/DOCX Parser + OCR
    │
    ▼
    Structure / Article Detection
    │
    ▼
    Chunking + Metadata
    │
    ▼
    Embeddings
    │
    ▼
    PostgreSQL + pgvector
    │
    ▼
    RAG Knowledge Base
