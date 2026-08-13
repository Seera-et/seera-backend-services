# Software Requirements Specification (SRS)

## AI-Powered Ethiopian Legal Intelligence Platform

**Document Version:** 1.0  
**Status:** MVP Specification  
**Standard Basis:** IEEE 830 / ISO/IEC/IEEE 29148  
**System Type:** Web-based AI Legal Intelligence Platform  
**Primary Region:** Ethiopia

---

# 1. Introduction

## 1.1 Purpose

This Software Requirements Specification defines the functional, non-functional, interface, data, security, performance, and verification requirements for an AI-powered Ethiopian Legal Intelligence Platform.

The system provides users with access to Ethiopian legal information through a combination of:

- Retrieval-Augmented Generation (RAG)
- Hybrid semantic and keyword search
- Large Language Models (LLMs)
- Structured legal document storage
- Deterministic business decision logic
- Document analysis
- Article-level citations and source grounding

The system is designed as an MVP that is both a usable legal-information product and a production-oriented engineering project demonstrating modern AI, backend, database, search, and document-processing architecture.

---

## 1.2 Scope

The system shall provide the following capabilities:

1. AI Legal Q&A
2. Ethiopian Legal Knowledge Explorer
3. Business & License Advisor
4. Document Intelligence
5. Citation & Source Grounding
6. Legal Document Ingestion and Content Management

The system shall use a **single unified legal knowledge base and RAG infrastructure** to support the AI features.

Authentication, payments, mobile applications, custom model fine-tuning, and automated legal document generation are outside the MVP scope.

---

## 1.3 Product Objectives

The system shall:

- Make Ethiopian legal information easier to discover.
- Allow users to ask questions using natural language.
- Provide answers grounded in indexed Ethiopian legal sources.
- Provide article-level citations.
- Allow users to browse and search legal documents.
- Guide entrepreneurs through business registration and licensing considerations.
- Allow users to upload and analyze legal documents.
- Preserve legal-document versions.
- Provide an extensible architecture for future AI models and data sources.

---

# 2. Definitions, Acronyms and Abbreviations

| Term | Definition |
|---|---|
| **AI** | Artificial Intelligence |
| **API** | Application Programming Interface |
| **RAG** | Retrieval-Augmented Generation |
| **LLM** | Large Language Model |
| **NLP** | Natural Language Processing |
| **OCR** | Optical Character Recognition |
| **PDF** | Portable Document Format |
| **DOCX** | Microsoft Word Open XML document |
| **SSE** | Server-Sent Events |
| **TTFT** | Time To First Token |
| **FTS** | Full-Text Search |
| **pgvector** | PostgreSQL extension for vector similarity search |
| **Embedding** | Numerical vector representation of text |
| **Chunk** | Searchable segment of a legal document |
| **Re-ranking** | Reordering retrieved search results based on relevance |
| **Precision@K** | Percentage of relevant results within the top K retrieved results |
| **HyDE** | Hypothetical Document Embeddings; retrieval technique using an LLM-generated hypothetical answer/document |
| **Grounding** | Ensuring generated claims are supported by retrieved source material |
| **Citation** | Reference from an AI response to a specific internal legal source |
| **Hybrid Search** | Combination of vector/semantic search and keyword search |
| **Document Version** | Specific published or historical version of a legal document |
| **SUT** | System Under Test |

---

# 3. Product Overview

## 3.1 Product Perspective

The system is a standalone web application consisting of:

```text
┌─────────────────────────────────────┐
│           Web Application           │
│       Next.js + React + TS          │
└──────────────────┬──────────────────┘
                   │
              REST + SSE
                   │
┌──────────────────▼──────────────────┐
│             Go API                   │
│       Application / RAG Layer        │
└──────────────────┬──────────────────┘
                   │
        ┌──────────┼───────────┐
        │          │           │
        ▼          ▼           ▼
   PostgreSQL   Object      AI Provider
   + pgvector   Storage      Abstraction
        │
        ▼
 Legal Knowledge Base
```

---

# 4. High-Level Architecture

## 4.1 User Request Architecture

```text
User
 │
 ▼
Next.js Web UI
 │
 ▼
Go REST API
 │
 ▼
Request Classification
 │
 ▼
Query Processing
 │
 ▼
Hybrid Retrieval
 ├───────────────┐
 ▼               ▼
Vector Search   PostgreSQL FTS
 │               │
 └───────┬───────┘
         ▼
   Candidate Merge
         │
         ▼
     Re-ranking
         │
         ▼
   Context Builder
         │
         ▼
        LLM
         │
         ▼
Grounding Validation
         │
         ▼
Answer + Citations
         │
         ▼
     SSE Stream
         │
         ▼
       User
```

---

# 5. Overall Description

## 5.1 Product Perspective

The platform consists of three primary interfaces:

### Public Web Interface

Used by:

- Entrepreneurs
- Legal professionals
- Researchers
- Students
- General users

### Administrative Interface

Used to:

- Upload legal documents
- Review extraction results
- Manage document versions
- Categorize documents
- Publish/unpublish legal sources
- Monitor ingestion status

### Backend API

Provides:

- Search APIs
- RAG APIs
- Document APIs
- Business Advisor APIs
- Citation APIs
- Ingestion APIs

---

# 6. User Classes

| User Class | Characteristics |
|---|---|
| **General User** | Wants understandable legal information |
| **Entrepreneur** | Needs business registration/licensing guidance |
| **Legal Professional** | Requires accurate source-level legal search |
| **Researcher/Student** | Needs document and article exploration |
| **System Administrator** | Manages platform configuration and content |
| **Legal Content Manager** | Uploads, validates, categorizes and publishes legal documents |

Authentication is not required for MVP users.

---

# 7. Operating Environment

## 7.1 Frontend

The system shall use:

- Next.js
- React
- TypeScript
- Tailwind CSS
- Modern browser APIs
- Server-side rendering where appropriate
- Client-side rendering for interactive AI interfaces

Supported browsers:

- Chrome
- Edge
- Firefox
- Safari

Latest two major versions should be targeted.

---

## 7.2 Backend

The backend shall use:

- Go
- REST APIs
- Server-Sent Events
- JSON request/response format
- OpenAPI-compatible API documentation

Recommended HTTP framework:

- Chi or Gin

---

## 7.3 Database

Minimum database requirements:

- PostgreSQL 15+
- pgvector extension
- PostgreSQL Full-Text Search

The database shall store structured legal information and embeddings.

---

## 7.4 Object Storage

Object storage shall contain:

- Original legal PDFs
- DOCX files
- User-uploaded documents
- Processed document artifacts where necessary

The storage layer should be S3-compatible.

Examples include:

- AWS S3
- Cloudflare R2
- Supabase Storage
- MinIO for local development

---

## 7.5 AI Infrastructure

The AI layer shall support:

- Hosted LLM APIs
- Hosted embedding APIs
- Local/self-hosted models in future

The application shall not directly couple business logic to a specific provider.

---

# 8. Assumptions and Dependencies

## 8.1 Assumptions

The system assumes:

1. Ethiopian legal documents can be legally obtained and processed.
2. Legal documents have sufficient text quality for extraction.
3. Some scanned documents may require OCR.
4. Legal documents may have multiple versions.
5. Legal information may change over time.
6. AI output must not be treated as formal legal advice.

---

## 8.2 Dependencies

The system depends on:

- PostgreSQL
- pgvector
- Object Storage
- LLM provider
- Embedding model
- PDF extraction library
- OCR engine
- Internet connectivity for hosted AI providers

---

# 9. Functional Requirements

# 9.1 Module A — AI Legal Q&A

## 9.1.1 Description

The system shall allow users to ask natural-language questions about Ethiopian law and receive answers grounded in the legal knowledge base.

---

## 9.1.2 Functional Requirements

### FR-QA-001

The system shall provide a natural-language question input.

### FR-QA-002

The system shall classify and normalize the user query.

### FR-QA-003

The system shall retrieve relevant legal content using hybrid search.

### FR-QA-004

The system shall combine vector search and keyword search results.

### FR-QA-005

The system shall re-rank retrieved legal chunks.

### FR-QA-006

The system shall construct an LLM context containing retrieved sources.

### FR-QA-007

The LLM shall generate an answer based on retrieved evidence.

### FR-QA-008

The system shall associate factual legal claims with citations where applicable.

### FR-QA-009

The system shall stream generated responses through SSE.

### FR-QA-010

If sufficient evidence cannot be found, the system shall indicate uncertainty instead of generating unsupported legal claims.

---

## Inputs

```text
User question
Optional conversation context
Optional selected legal document
```

## Outputs

```text
Streaming answer
Legal citations
Source metadata
Related legal documents
```

---

## Preconditions

- Legal knowledge base is available.
- At least one active LLM provider is configured.
- Embedding/search infrastructure is operational.

## Postconditions

- User receives an answer or an explicit insufficient-evidence response.
- Retrieved sources are recorded for citation generation.

---

# 9.2 Module B — Legal Knowledge Explorer

## 9.2.1 Description

The Knowledge Explorer shall provide searchable and browsable access to indexed Ethiopian legal documents.

---

## 9.2.2 Functional Requirements

### FR-EXP-001

Users shall be able to search legal documents using keywords.

### FR-EXP-002

Users shall be able to perform semantic searches.

### FR-EXP-003

Users shall filter results by:

- Legal domain
- Document type
- Year
- Status
- Language
- Source

### FR-EXP-004

Users shall browse documents by category.

### FR-EXP-005

Users shall open a document.

### FR-EXP-006

Users shall navigate to individual articles/sections.

### FR-EXP-007

The system shall display document metadata.

### FR-EXP-008

The system shall support historical document versions.

---

## Inputs

```text
Search query
Filters
Document ID
Article ID
```

## Outputs

```text
Search results
Document information
Article text
Metadata
Related sources
```

---

# 9.3 Module C — Business & License Advisor

## 9.3.1 Description

The Business Advisor shall guide users through a structured workflow and provide business structure, registration, and licensing recommendations grounded in Ethiopian legal sources.

---

## 9.3.2 Functional Requirements

### FR-BIZ-001

The system shall ask structured questions about the proposed business.

Example inputs:

- Business activity
- Number of founders
- Ownership
- Foreign participation
- Location
- Business type

### FR-BIZ-002

The system shall validate user inputs.

### FR-BIZ-003

The system shall apply deterministic business rules.

### FR-BIZ-004

The system shall retrieve relevant legal sources using RAG.

### FR-BIZ-005

The system shall combine deterministic rules and retrieved legal information.

### FR-BIZ-006

The system shall generate a recommendation.

### FR-BIZ-007

The system shall provide recommended registration steps.

### FR-BIZ-008

The system shall provide potential license requirements.

### FR-BIZ-009

The system shall cite the legal basis for recommendations.

---

## Processing

```text
User Inputs
     ↓
Validation
     ↓
Decision Rules
     ↓
Legal Query Generation
     ↓
RAG Retrieval
     ↓
Legal Evidence
     ↓
LLM Explanation
     ↓
Recommendation
```

---

## Output

```text
Recommended business structure
Potential licenses
Registration steps
Required information/documents
Reasoning
Legal sources
Caveats
```

---

# 9.4 Module D — Document Intelligence

## 9.4.1 Description

Users shall be able to upload PDF or DOCX files and interact with the content using AI.

---

## 9.4.2 Functional Requirements

### FR-DOC-001

The system shall accept PDF and DOCX files.

### FR-DOC-002

The system shall validate file type and size.

### FR-DOC-003

The system shall extract document text.

### FR-DOC-004

The system shall use OCR when text extraction is insufficient.

### FR-DOC-005

The system shall generate document chunks.

### FR-DOC-006

The system shall generate embeddings for document chunks.

### FR-DOC-007

The system shall allow users to request document summaries.

### FR-DOC-008

The system shall allow document-specific Q&A.

### FR-DOC-009

The system shall identify potentially relevant legal issues.

### FR-DOC-010

The system shall compare document content against relevant Ethiopian legal sources when performing compliance analysis.

---

## Processing Architecture

```text
User Document
      │
      ▼
Text Extraction
      │
      ▼
OCR if necessary
      │
      ▼
Document Structure
      │
      ▼
Temporary Chunk Index
      │
      ├───────────────┐
      ▼               ▼
User Document      Legal KB
Retrieval          Retrieval
      │               │
      └───────┬───────┘
              ▼
             LLM
              │
              ▼
      Analysis + Citations
```

---

# 9.5 Module E — Citation & Source Grounding

## 9.5.1 Description

Citation is a core platform capability shared by the AI modules.

---

## 9.5.2 Functional Requirements

### FR-CIT-001

The system shall assign unique identifiers to legal documents.

### FR-CIT-002

The system shall assign unique identifiers to document versions.

### FR-CIT-003

The system shall assign unique identifiers to articles and chunks.

### FR-CIT-004

AI-generated citations shall reference internal database records.

### FR-CIT-005

Users shall be able to open a citation.

### FR-CIT-006

The citation view shall display the relevant legal text.

### FR-CIT-007

The citation view shall display source metadata.

### FR-CIT-008

The system shall retain the original source URL when available.

### FR-CIT-009

The system shall distinguish current and historical versions.

---

## Citation Data Flow

```text
LLM Answer
    ↓
Citation IDs
    ↓
Database Lookup
    ↓
Document
    ↓
Version
    ↓
Article
    ↓
Exact Source Text
```

---

# 9.6 Module F — Legal Document Ingestion Pipeline

## 9.6.1 Description

The ingestion pipeline shall transform raw legal documents into structured searchable knowledge.

---

## 9.6.2 Ingestion Workflow

```text
Admin Upload
     ↓
File Validation
     ↓
Object Storage
     ↓
Text Extraction
     ↓
OCR if required
     ↓
Document Structure Detection
     ↓
Article/Section Parsing
     ↓
Chunking
     ↓
Metadata Assignment
     ↓
Embedding Generation
     ↓
Database Indexing
     ↓
Quality Review
     ↓
Publish
```

---

## Functional Requirements

### FR-ING-001

Admins shall be able to upload legal documents.

### FR-ING-002

The system shall validate uploaded files.

### FR-ING-003

The system shall store original files in private object storage.

### FR-ING-004

The system shall extract text.

### FR-ING-005

The system shall invoke OCR when required.

### FR-ING-006

The system shall identify legal structures such as:

- Chapter
- Part
- Section
- Article
- Paragraph
- Clause

### FR-ING-007

The system shall create searchable chunks.

### FR-ING-008

The system shall attach metadata.

### FR-ING-009

The system shall generate embeddings.

### FR-ING-010

The system shall index chunks in PostgreSQL/pgvector.

### FR-ING-011

Admins shall be able to review processing results.

### FR-ING-012

Admins shall be able to publish or reject documents.

### FR-ING-013

The system shall preserve previous versions.

---

# 10. Legal Document Data Requirements

Each legal document should contain at minimum:

```text
Document ID
Title
Document Type
Legal Domain
Issuing Authority
Publication Date
Effective Date
Language
Status
Source URL
Storage Location
Current Version
```

Each article should contain:

```text
Article ID
Document ID
Version ID
Chapter
Section
Article Number
Title
Text
Order
```

Each searchable chunk should contain:

```text
Chunk ID
Article ID
Document ID
Version ID
Text
Embedding
Token Count
Metadata
```

---

# 11. External Interface Requirements

# 11.1 User Interface

## Chat Interface

The UI shall include:

- Message history
- Question input
- Streaming response
- Loading state
- Citation markers
- Source cards
- Related documents
- Error states

---

## Legal Explorer

The UI shall include:

- Search field
- Filters
- Category navigation
- Document cards
- Article navigation
- Source viewer

---

## Business Advisor

The UI shall include:

- Step-based workflow
- Progress indicator
- Input controls
- Validation messages
- Recommendation summary
- Registration checklist
- Citation cards

---

## Document Intelligence

The UI shall include:

- File upload
- Upload progress
- Document preview
- Summary
- Q&A interface
- Legal issue/risk section
- Source citations

---

## Citation Modal/Panel

A citation interaction shall display:

```text
Document Title
Issuing Authority
Version
Effective Date
Article Number

─────────────────────

Exact Legal Text

─────────────────────

Original Source
```

---

# 12. Software Interfaces

## 12.1 PostgreSQL

The backend shall communicate with PostgreSQL using a Go PostgreSQL driver.

Recommended options:

- pgx
- database/sql

---

## 12.2 Vector Search

The system shall use pgvector for:

- Embedding storage
- Cosine similarity
- Vector nearest-neighbor search

---

## 12.3 LLM Provider

The backend shall define an abstraction similar to:

```text
LLMProvider

Generate()
Stream()
GenerateStructured()
```

Provider implementations may include:

```text
OpenAIProvider
AnthropicProvider
LocalModelProvider
```

---

## 12.4 Embedding Provider

The system shall define:

```text
EmbeddingProvider

EmbedText()
EmbedBatch()
```

Implementations may include:

```text
OpenAIEmbeddingProvider
JinaEmbeddingProvider
LocalEmbeddingProvider
```

---

## 12.5 Document Processing

The ingestion system shall support:

- PyMuPDF
- OCR engine
- DOCX parser

Document processing may run as a separate Python worker/service while the primary API remains Go.

---

# 13. Communication Protocols

## REST

The API shall use HTTP/HTTPS and JSON.

Example endpoints:

```text
GET    /api/v1/legal/search
GET    /api/v1/legal/documents/:id
GET    /api/v1/legal/articles/:id

POST   /api/v1/qa/query
POST   /api/v1/business/advisor
POST   /api/v1/documents/upload
POST   /api/v1/documents/:id/query

GET    /api/v1/citations/:id
```

---

## SSE

AI streaming endpoints shall use:

```text
Content-Type: text/event-stream
```

Example:

```text
event: token
data: Ethiopia

event: token
data: 's

event: citation
data: {"id":"citation_123"}

event: done
data: {}
```

SSE is preferred for MVP AI streaming because communication is primarily server → client.

---

# 14. Non-Functional Requirements

# 14.1 Performance

| Requirement | Target |
|---|---:|
| Hybrid search | < 1 second |
| TTFT | < 3 seconds |
| Complete AI response | < 10 seconds |
| Citation retrieval | < 300 ms |
| Typical PDF processing | < 60 seconds |
| API response for normal CRUD | < 500 ms |
| Search pagination | < 1 second |

Performance measurements shall be conducted under a representative MVP workload.

---

# 15. Accuracy Requirements

## 15.1 Retrieval

The system shall achieve:

**Precision@5 ≥ 80%**

on the predefined legal evaluation dataset.

---

## 15.2 Citation Grounding

At least:

**90% citation grounding accuracy**

shall be achieved.

A citation is considered correct when the referenced legal source actually supports the associated claim.

---

## 15.3 Legal Hallucination

For cited legal claims:

> **Zero tolerance for knowingly unsupported legal claims in the acceptance test set.**

The system shall prefer:

```text
"I don't have sufficient evidence..."
```

over an unsupported legal answer.

---

# 16. Reliability Requirements

The system shall:

- Handle unavailable LLM providers gracefully.
- Handle embedding service failures.
- Retry transient external API failures.
- Avoid duplicate document ingestion.
- Preserve document versions.
- Prevent partial publication of failed ingestion jobs.
- Log ingestion and RAG failures.

---

# 17. Security Requirements

## 17.1 API Security

The API shall implement:

- Input validation
- Request size limits
- Rate limiting
- File type validation
- File size restrictions
- Secure API key storage
- HTTPS in production

---

## 17.2 Uploaded Document Security

Uploaded files shall:

- Be stored privately.
- Never be exposed through predictable public URLs.
- Have generated storage identifiers.
- Be isolated from other uploaded documents.
- Be automatically cleaned according to retention policy.

---

## 17.3 MVP Document Retention

For unauthenticated MVP users:

```text
Upload
  ↓
Temporary storage
  ↓
Processing
  ↓
Analysis
  ↓
Automatic deletion
```

Recommended initial retention:

**24 hours maximum**, unless explicitly required for an active processing workflow.

---

# 18. Data Isolation

The system shall logically isolate:

1. Official legal documents.
2. Published legal knowledge.
3. User-uploaded documents.
4. Temporary processing data.

User-uploaded documents must never become part of the global legal knowledge base without an explicit administrative ingestion process.

---

# 19. Database Integrity

The system shall use:

- Foreign-key constraints
- Unique constraints
- Transactions
- Appropriate indexes

Document publishing shall be transactional.

For example:

```text
Create Version
      ↓
Create Articles
      ↓
Create Chunks
      ↓
Create Embeddings
      ↓
Validate
      ↓
Publish
```

If critical processing fails, the incomplete version shall not become active.

Database-level locking/version checks shall prevent conflicting updates to the same legal document version.

---

# 20. Maintainability

The system shall follow modular architecture.

Recommended backend structure:

```text
/internal

  /api
  /domain
  /application
  /repository
  /rag
  /search
  /citation
  /business
  /document
  /llm
  /embedding
  /storage
```

The architecture shall separate:

- Business logic
- Infrastructure
- AI providers
- Database
- HTTP/API layer

---

# 21. LLM Provider Portability

The system shall not directly couple business logic to a specific LLM vendor.

```text
                    Application
                        │
                  LLM Interface
                        │
          ┌─────────────┼──────────────┐
          ▼             ▼              ▼
       OpenAI        Anthropic      Local LLM
```

Changing providers shall not require rewriting:

- RAG logic
- Business Advisor
- Citation system
- API contracts

---

# 22. Observability

The system shall log:

- Request ID
- API latency
- Retrieval latency
- Number of retrieved chunks
- Re-ranking latency
- LLM latency
- Token usage where available
- Citation IDs
- Ingestion status
- Processing errors

Sensitive document contents shall not be unnecessarily written to application logs.

---

# 23. Verification & Validation

# 23.1 MVP Acceptance Criteria

The MVP shall pass all of the following.

### AC-001 — Legal Q&A

**Given** an indexed legal question with known source articles,

**When** the user asks the question,

**Then** the system shall:

- Return an answer.
- Stream the answer.
- Provide relevant citations.
- Identify the source article.

**Pass condition:** ≥ 90% of evaluation questions produce correctly grounded answers.

---

### AC-002 — Knowledge Explorer

**Given** an indexed legal document,

**When** the user searches by title, keyword, or article number,

**Then** the correct document/article shall appear.

**Pass condition:** ≥ 95% retrieval success for predefined exact-search tests.

---

### AC-003 — Business Advisor

**Given** a predefined business scenario,

**When** the user completes the workflow,

**Then** the system shall return:

- Business structure recommendation.
- Relevant registration steps.
- Potential license requirements.
- Supporting legal citations.

**Pass condition:** 100% of predefined MVP scenarios produce the expected rule-based recommendation.

---

### AC-004 — Document Intelligence

**Given** a valid PDF/DOCX,

**When** the user uploads it,

**Then** the system shall:

- Process the document.
- Generate a summary.
- Support document-specific Q&A.
- Retrieve relevant Ethiopian legal sources for compliance analysis.

**Pass condition:** ≥ 95% successful processing for supported test documents.

---

### AC-005 — Citation System

**Given** an AI response containing citations,

**When** the user selects a citation,

**Then** the application shall display:

- Correct document.
- Correct version.
- Correct article.
- Exact relevant source text.

**Pass condition:** ≥ 90% citation correctness and 100% citation resolvability.

---

### AC-006 — Ingestion Pipeline

**Given** a valid legal PDF,

**When** an administrator uploads and publishes it,

**Then** the system shall:

```text
Extract
→ Structure
→ Chunk
→ Embed
→ Index
→ Publish
```

**Pass condition:** 95% of valid test documents successfully complete the pipeline.

---

# 24. Evaluation Dataset

A dedicated legal evaluation dataset shall be created.

Example:

```text
Question
Expected Document
Expected Article
Expected Supporting Text
Expected Answer
```

Example:

```text
Question:
"What are the requirements for forming a private limited company?"

Expected:
Document: Commercial Code
Article: XX
```

The dataset should contain at least:

- 50 Legal Q&A questions
- 20 Knowledge Explorer queries
- 10 Business Advisor scenarios
- 10 Document Intelligence test documents/questions
- 20 citation-grounding tests

Total minimum:

**110 evaluation cases.**

---

# 25. RAG Evaluation

The RAG pipeline shall be evaluated independently from the LLM.

Metrics:

```text
Retrieval Precision@5
Retrieval Recall@K
Citation Accuracy
Grounding Rate
Answer Relevance
```

This separation is important because a bad answer can result from either:

```text
Bad Retrieval
      OR
Bad Generation
```

The evaluation framework must identify which component failed.

---

# 26. End-to-End MVP Flow

```text
                         USER
                           │
                           ▼
                    Next.js Web App
                           │
                           ▼
                      Go REST API
                           │
          ┌────────────────┼────────────────┐
          │                │                │
          ▼                ▼                ▼
      Legal Q&A       Business Advisor   Document AI
          │                │                │
          └────────────────┼────────────────┘
                           │
                           ▼
                       RAG Engine
                           │
                  ┌────────┴────────┐
                  ▼                 ▼
             Vector Search      Keyword Search
             pgvector           PostgreSQL FTS
                  │                 │
                  └────────┬────────┘
                           ▼
                       Re-ranker
                           │
                           ▼
                          LLM
                           │
                           ▼
                  Grounding Validator
                           │
                           ▼
                    Answer + Citations
                           │
                           ▼
                         SSE
                           │
                           ▼
                         USER


ADMIN
  │
  ▼
Legal Document
  │
  ▼
PDF/DOCX Processing
  │
  ▼
OCR / Text Extraction
  │
  ▼
Legal Structure Parsing
  │
  ▼
Article/Section Chunking
  │
  ▼
Metadata
  │
  ▼
Embeddings
  │
  ▼
PostgreSQL + pgvector
  │
  ▼
Published Legal Knowledge Base
```

---

# 27. MVP Technical Definition

The MVP shall consist of:

### Frontend

```text
Next.js
React
TypeScript
Tailwind CSS
SSE Client
```

### Backend

```text
Go
REST API
SSE
RAG Service
Business Rule Engine
Citation Service
```

### Database

```text
PostgreSQL 15+
pgvector
PostgreSQL Full-Text Search
```

### Storage

```text
S3-compatible Object Storage
```

### AI

```text
LLM Provider Abstraction
Embedding Provider Abstraction
Re-ranking component
Grounding validation
```

### Document Processing

```text
PyMuPDF
DOCX Parser
OCR
Legal Structure Parser
```

---

# 28. MVP Completion Criteria

The MVP shall be considered technically complete when:

- [ ] Ethiopian legal documents can be ingested.
- [ ] PDF/DOCX text extraction works.
- [ ] OCR works for scanned documents.
- [ ] Legal articles can be identified and indexed.
- [ ] Embeddings are stored in pgvector.
- [ ] PostgreSQL keyword search works.
- [ ] Hybrid retrieval works.
- [ ] Re-ranking works.
- [ ] LLM provider abstraction works.
- [ ] AI answers stream through SSE.
- [ ] AI answers contain source citations.
- [ ] Citations resolve to exact internal legal records.
- [ ] Legal documents can be browsed.
- [ ] Business Advisor workflow works.
- [ ] Business Advisor combines deterministic rules with RAG.
- [ ] PDF/DOCX Document Intelligence works.
- [ ] Uploaded documents remain isolated from official legal data.
- [ ] Document versions are preserved.
- [ ] Rate limiting and file validation are implemented.
- [ ] RAG evaluation dataset exists.
- [ ] Precision@5 reaches ≥80%.
- [ ] Citation grounding reaches ≥90%.
- [ ] TTFT is <3 seconds under representative conditions.
- [ ] Typical complete AI responses are <10 seconds.
- [ ] Legal disclaimer is visible to users.

---

# 29. MVP Architectural Principle

The central architectural principle is:

> **Build one reusable legal intelligence infrastructure instead of implementing separate AI systems for every feature.**

The shared platform consists of:

```text
                ┌─────────────────────┐
                │ Legal Knowledge Base│
                └──────────┬──────────┘
                           │
                    Unified RAG Engine
                           │
        ┌──────────────────┼──────────────────┐
        │                  │                  │
        ▼                  ▼                  ▼
    Legal Q&A        Business Advisor    Document AI
        │                  │                  │
        └──────────────────┼──────────────────┘
                           ▼
                   Citation System
```

The result is an extensible system in which future capabilities—such as Amharic legal search, additional legal domains, APIs, automated legal workflows, or alternative LLMs—can be added without replacing the core architecture.